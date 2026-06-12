package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ieaster/lazysystemd/pkg/gocui"
)

const (
	viewUnits  = "units"
	viewDetail = "detail"
	viewProc   = "proc"
	viewLogs   = "logs"
	viewStatus = "status"
	viewFilter = "filter"
)

var (
	panes      = []string{viewUnits, viewDetail, viewProc, viewLogs}
	roundedBox = []rune{'─', '│', '╭', '╮', '╰', '╯', '├', '┤', '┬', '┴', '┼'}
	unitKinds  = []unitKind{
		{label: "services", systemctlTypes: []string{"service"}},
		{label: "sockets", systemctlTypes: []string{"socket"}},
		{label: "timers", systemctlTypes: []string{"timer"}},
		{label: "mounts", systemctlTypes: []string{"mount"}},
		{label: "all", systemctlTypes: []string{"service", "socket", "timer", "mount"}},
	}
	actions = []unitActionBinding{
		{key: 's', action: "start"},
		{key: 'x', action: "stop"},
		{key: 'R', action: "restart"},
		{key: 'e', action: "enable"},
		{key: 'd', action: "disable"},
		{key: 'm', action: "mask"},
		{key: 'M', action: "unmask"},
	}
)

type unitKind struct {
	label          string
	systemctlTypes []string
}

type unitActionBinding struct {
	key    rune
	action string
}

type unit struct {
	Name        string
	Load        string
	Active      string
	Sub         string
	Description string
}

type runner struct {
	userMode bool
}

func (r runner) systemctl(ctx context.Context, args ...string) (string, error) {
	baseArgs := []string{}
	if r.userMode {
		baseArgs = append(baseArgs, "--user")
	}
	baseArgs = append(baseArgs, args...)

	cmd := exec.CommandContext(ctx, "systemctl", baseArgs...)
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

func (r runner) journalctl(ctx context.Context, unitName string) (string, error) {
	args := []string{"-u", unitName, "-n", "200", "--no-pager", "--output", "short-iso"}
	if r.userMode {
		args = append([]string{"--user"}, args...)
	}

	cmd := exec.CommandContext(ctx, "journalctl", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

func (r runner) ps(ctx context.Context, pid string) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", "-p", pid, "-o", "pid,ppid,%cpu,%mem,rss,etime,stat,comm,args", "--no-headers")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type app struct {
	g      *gocui.Gui
	mu     sync.Mutex
	runner runner

	units          []unit
	selected       int
	unitOffset     int
	detailOffset   int
	procOffset     int
	logOffset      int
	filter         string
	filtering      bool
	focusedPane    string
	kindIndex      int
	status         string
	detail         string
	proc           string
	logs           string
	loading        bool
	selectionToken int
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode:      gocui.OutputTrue,
		SupportOverlaps: true,
	})
	if err != nil {
		return err
	}
	defer g.Close()

	a := &app{
		g:           g,
		focusedPane: viewUnits,
		status:      "Loading units...",
	}

	g.Highlight = true
	g.SelFgColor = gocui.ColorGreen
	g.SelFrameColor = gocui.ColorGreen
	g.SetManagerFunc(a.layout)
	a.bindKeys()
	a.refresh()

	err = g.MainLoop()
	if errors.Is(err, gocui.ErrQuit) {
		return nil
	}
	return err
}

func (a *app) bindKeys() {
	quit := func(*gocui.Gui, *gocui.View) error {
		if a.closeFilter() {
			return nil
		}
		return gocui.ErrQuit
	}

	a.g.SetKeybinding("", gocui.NewKeyRune('q'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('q') {
			return nil
		}
		return quit(nil, nil)
	})
	a.g.SetKeybinding("", gocui.NewKeyStrMod("c", gocui.ModCtrl), quit)
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyEsc), quit)
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyEnter), func(*gocui.Gui, *gocui.View) error {
		a.closeFilter()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyBackspace), func(*gocui.Gui, *gocui.View) error {
		if a.filteringBackspace() {
			return nil
		}
		return gocui.ErrKeybindingNotHandled
	})

	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyArrowLeft), func(*gocui.Gui, *gocui.View) error {
		return a.focusPrevious()
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('h'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('h') {
			return nil
		}
		return a.focusPrevious()
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyArrowRight), func(*gocui.Gui, *gocui.View) error {
		return a.focusNext()
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('l'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('l') {
			return nil
		}
		return a.focusNext()
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyTab), func(*gocui.Gui, *gocui.View) error {
		return a.focusNext()
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyBacktab), func(*gocui.Gui, *gocui.View) error {
		return a.focusPrevious()
	})

	for i, pane := range panes {
		i, pane := i, pane
		a.g.SetKeybinding("", gocui.NewKeyRune(rune('1'+i)), func(*gocui.Gui, *gocui.View) error {
			if a.filteringRune(rune('1' + i)) {
				return nil
			}
			a.focusPane(pane)
			return nil
		})
	}

	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyArrowUp), func(*gocui.Gui, *gocui.View) error {
		return a.moveOrScroll(-1)
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('k'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('k') {
			return nil
		}
		return a.moveOrScroll(-1)
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyArrowDown), func(*gocui.Gui, *gocui.View) error {
		return a.moveOrScroll(1)
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('j'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('j') {
			return nil
		}
		return a.moveOrScroll(1)
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyPgup), func(*gocui.Gui, *gocui.View) error {
		return a.moveOrScroll(-10)
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyPgdn), func(*gocui.Gui, *gocui.View) error {
		return a.moveOrScroll(10)
	})

	a.g.SetKeybinding("", gocui.NewKeyRune('['), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('[') {
			return nil
		}
		a.cycleKind(-1)
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune(']'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune(']') {
			return nil
		}
		a.cycleKind(1)
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('/'), func(*gocui.Gui, *gocui.View) error {
		a.openFilter()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('r'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('r') {
			return nil
		}
		a.refresh()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('u'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('u') {
			return nil
		}
		a.toggleUserMode()
		return nil
	})
	for _, binding := range actions {
		a.g.SetKeybinding("", gocui.NewKeyRune(binding.key), a.unitAction(binding.key, binding.action))
	}

	for ch := rune(32); ch <= 126; ch++ {
		if ch == '/' {
			continue
		}
		ch := ch
		a.g.SetKeybinding("", gocui.NewKeyRune(ch), func(*gocui.Gui, *gocui.View) error {
			if a.filteringRune(ch) {
				return nil
			}
			return gocui.ErrKeybindingNotHandled
		})
	}
}

func (a *app) layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()
	if maxX < 70 || maxY < 20 {
		return nil
	}

	statusHeight := 2
	bodyBottom := maxY - statusHeight - 1
	leftW := maxX / 3
	if leftW < 36 {
		leftW = 36
	}
	if leftW > maxX-42 {
		leftW = maxX - 42
	}

	detailBottom := bodyBottom / 3
	procBottom := detailBottom + (bodyBottom-detailBottom)/3

	units, err := g.SetView(viewUnits, 0, 0, leftW-1, bodyBottom, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	detail, err := g.SetView(viewDetail, leftW, 0, maxX-1, detailBottom, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	proc, err := g.SetView(viewProc, leftW, detailBottom+1, maxX-1, procBottom, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	logs, err := g.SetView(viewLogs, leftW, procBottom+1, maxX-1, bodyBottom, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	status, err := g.SetView(viewStatus, 0, bodyBottom+1, maxX-1, maxY-1, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, view := range []*gocui.View{units, detail, proc, logs} {
		view.FrameRunes = roundedBox
		view.Highlight = false
		view.Footer = ""
	}
	status.Frame = false

	units.Title = a.titleFor(viewUnits, "Units")
	detail.Title = a.titleFor(viewDetail, "Unit Details")
	proc.Title = a.titleFor(viewProc, "Proc Details")
	logs.Title = a.titleFor(viewLogs, "Journal")

	filtered := a.filteredUnits()
	a.clampSelection(len(filtered))
	a.keepSelectionVisible(units.InnerHeight())

	units.SetContent(a.unitsContent(filtered, units.InnerWidth(), units.InnerHeight()))
	if len(filtered) > 0 {
		units.SetHighlight(a.selected-a.unitOffset+1, true)
	}
	a.detailOffset = clampScrollOffset(a.detailOffset, a.detail, detail.InnerHeight())
	a.procOffset = clampScrollOffset(a.procOffset, a.proc, proc.InnerHeight())
	a.logOffset = clampScrollOffset(a.logOffset, a.logs, logs.InnerHeight())
	detail.SetContent(visibleLines(emptyIfBlank(a.detail, "No unit selected."), a.detailOffset, detail.InnerHeight()))
	proc.SetContent(visibleLines(emptyIfBlank(a.proc, "No process data loaded."), a.procOffset, proc.InnerHeight()))
	logs.SetContent(visibleLines(emptyIfBlank(a.logs, "No journal entries loaded."), a.logOffset, logs.InnerHeight()))
	status.SetContent(a.statusLine(len(filtered)))

	if a.filtering {
		if err := a.renderFilterPrompt(g, maxX, maxY); err != nil {
			return err
		}
	} else if err := g.DeleteView(viewFilter); err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}

	if _, err := g.SetCurrentView(a.focusedPane); err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	return nil
}

func (a *app) renderFilterPrompt(g *gocui.Gui, maxX int, maxY int) error {
	width := min(max(34, len(a.filter)+12), maxX-8)
	height := 4
	x0 := (maxX - width) / 2
	y0 := (maxY - height) / 2
	filter, err := g.SetView(viewFilter, x0, y0, x0+width, y0+height, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	filter.FrameRunes = roundedBox
	filter.Title = " Filter Units "
	filter.SetContent("\n  / " + a.filter)
	_, err = g.SetViewOnTop(viewFilter)
	return err
}

func (a *app) refresh() {
	a.mu.Lock()
	a.loading = true
	a.status = "Loading " + a.currentKind().label + "..."
	types := a.currentKind().systemctlTypes
	a.mu.Unlock()
	a.render()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		args := []string{"list-units", "--all"}
		for _, systemctlType := range types {
			args = append(args, "--type", systemctlType)
		}
		args = append(args, "--no-legend", "--no-pager", "--plain")
		out, err := a.runner.systemctl(ctx, args...)

		units := parseUnits(out)
		a.mu.Lock()
		a.loading = false
		if err != nil {
			a.status = fmt.Sprintf("systemctl list-units failed: %s", strings.TrimSpace(out))
		} else {
			a.units = units
			sort.SliceStable(a.units, func(i, j int) bool {
				if a.units[i].Active == a.units[j].Active {
					return a.units[i].Name < a.units[j].Name
				}
				return a.units[i].Active < a.units[j].Active
			})
			a.clampSelection(len(a.filteredUnits()))
			a.status = fmt.Sprintf("Loaded %d %s.", len(a.units), a.currentKind().label)
		}
		a.mu.Unlock()
		a.loadSelected()
	}()
}

func (a *app) loadSelected() {
	unitName := a.selectedUnitName()
	a.mu.Lock()
	a.selectionToken++
	token := a.selectionToken
	a.mu.Unlock()

	if unitName == "" {
		a.mu.Lock()
		a.detail = ""
		a.proc = ""
		a.logs = ""
		a.detailOffset = 0
		a.procOffset = 0
		a.logOffset = 0
		a.mu.Unlock()
		a.render()
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		detail, detailErr := a.runner.systemctl(ctx,
			"show",
			unitName,
			"--no-pager",
			"--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState,FragmentPath,MainPID,ControlPID,ExecMainPID,Type,Result,NRestarts,TasksCurrent,MemoryCurrent,CPUUsageNSec,ActiveEnterTimestamp,ActiveExitTimestamp",
		)
		logs, logsErr := a.runner.journalctl(ctx, unitName)
		props := parseProperties(detail)

		a.mu.Lock()
		if token != a.selectionToken {
			a.mu.Unlock()
			return
		}
		if detailErr != nil {
			a.detail = fmt.Sprintf("systemctl show failed:\n%s", detail)
		} else {
			a.detail = formatUnitDetails(props)
		}
		if logsErr != nil {
			a.logs = fmt.Sprintf("journalctl failed:\n%s", logs)
		} else {
			a.logs = logs
		}
		a.detailOffset = 0
		a.procOffset = 0
		a.logOffset = 0
		a.mu.Unlock()

		a.refreshProc(token, unitName)
		a.render()
		a.startProcLoop(token, unitName)
	}()
}

func (a *app) startProcLoop(token int, unitName string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		a.mu.Lock()
		valid := token == a.selectionToken
		a.mu.Unlock()
		if !valid {
			return
		}
		a.refreshProc(token, unitName)
	}
}

func (a *app) refreshProc(token int, unitName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := a.runner.systemctl(ctx,
		"show",
		unitName,
		"--no-pager",
		"--property=MainPID,ControlPID,ExecMainPID,NRestarts,TasksCurrent,MemoryCurrent,CPUUsageNSec,ActiveEnterTimestamp",
	)
	props := parseProperties(out)
	proc := formatProcDetails(props, "")
	if err == nil {
		if pid := props["MainPID"]; pid != "" && pid != "0" {
			if psOut, psErr := a.runner.ps(ctx, pid); psErr == nil {
				proc = formatProcDetails(props, psOut)
			}
		}
	} else {
		proc = fmt.Sprintf("systemctl show failed:\n%s", out)
	}

	a.mu.Lock()
	if token == a.selectionToken {
		a.proc = proc
	}
	a.mu.Unlock()
	a.render()
}

func (a *app) moveOrScroll(delta int) error {
	if a.isFiltering() {
		return nil
	}

	a.mu.Lock()
	focusedPane := a.focusedPane
	a.mu.Unlock()

	switch focusedPane {
	case viewUnits:
		a.moveSelection(delta)
	case viewDetail:
		a.scrollDetail(delta)
	case viewProc:
		a.scrollProc(delta)
	case viewLogs:
		a.scrollLogs(delta)
	}
	return nil
}

func (a *app) moveSelection(delta int) {
	a.mu.Lock()
	filtered := a.filteredUnits()
	if len(filtered) == 0 {
		a.mu.Unlock()
		return
	}
	a.selected += delta
	a.clampSelection(len(filtered))
	a.mu.Unlock()
	a.loadSelected()
	a.render()
}

func (a *app) scrollDetail(delta int) {
	a.mu.Lock()
	a.detailOffset = clampScrollOffset(a.detailOffset+delta, a.detail, 1)
	a.mu.Unlock()
	a.render()
}

func (a *app) scrollProc(delta int) {
	a.mu.Lock()
	a.procOffset = clampScrollOffset(a.procOffset+delta, a.proc, 1)
	a.mu.Unlock()
	a.render()
}

func (a *app) scrollLogs(delta int) {
	a.mu.Lock()
	a.logOffset = clampScrollOffset(a.logOffset+delta, a.logs, 1)
	a.mu.Unlock()
	a.render()
}

func (a *app) focusPrevious() error {
	if a.isFiltering() {
		return nil
	}
	a.moveFocus(-1)
	return nil
}

func (a *app) focusNext() error {
	if a.isFiltering() {
		return nil
	}
	a.moveFocus(1)
	return nil
}

func (a *app) focusPane(pane string) {
	if a.isFiltering() {
		return
	}
	a.mu.Lock()
	a.focusedPane = pane
	a.status = fmt.Sprintf("Focused %s.", paneLabel(a.focusedPane))
	a.mu.Unlock()
	a.render()
}

func (a *app) moveFocus(delta int) {
	a.mu.Lock()
	idx := paneIndex(a.focusedPane) + delta
	idx = clamp(idx, 0, len(panes)-1)
	a.focusedPane = panes[idx]
	a.status = fmt.Sprintf("Focused %s.", paneLabel(a.focusedPane))
	a.mu.Unlock()
	a.render()
}

func (a *app) cycleKind(delta int) {
	a.mu.Lock()
	a.kindIndex = (a.kindIndex + delta + len(unitKinds)) % len(unitKinds)
	a.selected = 0
	a.unitOffset = 0
	a.detailOffset = 0
	a.procOffset = 0
	a.logOffset = 0
	a.detail = ""
	a.proc = ""
	a.logs = ""
	a.status = "Switched to " + a.currentKind().label + "."
	a.mu.Unlock()
	a.refresh()
}

func (a *app) toggleUserMode() {
	a.mu.Lock()
	a.runner.userMode = !a.runner.userMode
	mode := modeName(a.runner.userMode)
	a.status = "Switched to " + mode + " units."
	a.selected = 0
	a.unitOffset = 0
	a.detailOffset = 0
	a.procOffset = 0
	a.logOffset = 0
	a.detail = ""
	a.proc = ""
	a.logs = ""
	a.mu.Unlock()
	a.refresh()
}

func (a *app) unitAction(key rune, action string) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune(key) {
			return nil
		}

		unitName := a.selectedUnitName()
		if unitName == "" {
			return nil
		}

		a.mu.Lock()
		a.status = fmt.Sprintf("Running systemctl %s %s...", action, unitName)
		a.mu.Unlock()
		a.render()

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := a.runner.systemctl(ctx, action, unitName)
			a.mu.Lock()
			if err != nil {
				a.status = fmt.Sprintf("systemctl %s failed: %s", action, strings.TrimSpace(out))
			} else if strings.TrimSpace(out) == "" {
				a.status = fmt.Sprintf("systemctl %s %s completed.", action, unitName)
			} else {
				a.status = strings.TrimSpace(out)
			}
			a.mu.Unlock()
			a.refresh()
		}()
		return nil
	}
}

func (a *app) selectedUnitName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	filtered := a.filteredUnits()
	if a.selected < 0 || a.selected >= len(filtered) {
		return ""
	}
	return filtered[a.selected].Name
}

func (a *app) filteredUnits() []unit {
	if a.filter == "" {
		return a.units
	}

	result := make([]unit, 0, len(a.units))
	filter := strings.ToLower(a.filter)
	for _, unit := range a.units {
		if strings.Contains(strings.ToLower(unit.Name), filter) ||
			strings.Contains(strings.ToLower(unit.Active), filter) ||
			strings.Contains(strings.ToLower(unit.Sub), filter) ||
			strings.Contains(strings.ToLower(unit.Description), filter) {
			result = append(result, unit)
		}
	}
	return result
}

func (a *app) unitsContent(units []unit, width int, height int) string {
	if len(units) == 0 {
		if a.loading {
			return "Loading..."
		}
		if a.filter != "" {
			return "No units match " + a.filter
		}
		return "No units found."
	}

	pageSize := unitPageSize(height)
	end := min(a.unitOffset+pageSize, len(units))

	var b strings.Builder
	b.WriteString(unitHeader(width))
	for i := a.unitOffset; i < end; i++ {
		fmt.Fprintln(&b, unitLine(units[i], width, i == a.selected))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *app) keepSelectionVisible(height int) {
	pageSize := unitPageSize(height)
	if a.selected < a.unitOffset {
		a.unitOffset = a.selected
	}
	if a.selected >= a.unitOffset+pageSize {
		a.unitOffset = a.selected - pageSize + 1
	}
	a.unitOffset = max(a.unitOffset, 0)
}

func (a *app) clampSelection(count int) {
	if count <= 0 {
		a.selected = 0
		a.unitOffset = 0
		return
	}
	a.selected = clamp(a.selected, 0, count-1)
}

func (a *app) openFilter() {
	a.mu.Lock()
	if a.filtering {
		a.filter += "/"
	} else {
		a.filtering = true
		a.focusedPane = viewUnits
	}
	a.selected = 0
	a.unitOffset = 0
	a.status = "Filtering units."
	a.mu.Unlock()
	a.render()
}

func (a *app) filteringRune(ch rune) bool {
	a.mu.Lock()
	if !a.filtering {
		a.mu.Unlock()
		return false
	}
	a.filter += string(ch)
	a.selected = 0
	a.unitOffset = 0
	a.status = "Filtering units."
	a.mu.Unlock()
	a.render()
	a.loadSelected()
	return true
}

func (a *app) filteringBackspace() bool {
	a.mu.Lock()
	if !a.filtering {
		a.mu.Unlock()
		return false
	}
	if a.filter != "" {
		a.filter = a.filter[:len(a.filter)-1]
	}
	a.selected = 0
	a.unitOffset = 0
	a.status = "Filtering units."
	a.mu.Unlock()
	a.render()
	a.loadSelected()
	return true
}

func (a *app) closeFilter() bool {
	a.mu.Lock()
	if !a.filtering {
		a.mu.Unlock()
		return false
	}
	a.filtering = false
	a.status = "Filter applied."
	a.mu.Unlock()
	a.render()
	return true
}

func (a *app) isFiltering() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.filtering
}

func (a *app) currentKind() unitKind {
	return unitKinds[a.kindIndex]
}

func (a *app) titleFor(viewName string, label string) string {
	if a.focusedPane == viewName {
		return " " + label + " * "
	}
	return " " + label + " "
}

func (a *app) statusLine(filteredCount int) string {
	mode := modeName(a.runner.userMode)
	filter := "none"
	if a.filter != "" {
		filter = a.filter
	}
	return fmt.Sprintf(" %s | %s | %d/%d units | filter: %s | %s | %s",
		mode,
		a.currentKind().label,
		filteredCount,
		len(a.units),
		filter,
		a.contextHelp(),
		a.status,
	)
}

func (a *app) contextHelp() string {
	if a.filtering {
		return "type filter | enter/esc close"
	}

	switch a.focusedPane {
	case viewUnits:
		return "j/k move | h/l panes | [/]/ kind | / filter | s/x/R service"
	case viewDetail:
		return "j/k scroll | h/l panes | 1-4 jump"
	case viewProc:
		return "j/k scroll | live process stats | 1-4 jump"
	case viewLogs:
		return "j/k scroll | pgup/pgdn page | r refresh"
	default:
		return "h/l panes | q quit"
	}
}

func (a *app) render() {
	a.g.Update(func(*gocui.Gui) error { return nil })
}

func parseUnits(out string) []unit {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	units := make([]unit, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		description := ""
		if len(fields) > 4 {
			description = strings.Join(fields[4:], " ")
		}

		units = append(units, unit{
			Name:        fields[0],
			Load:        fields[1],
			Active:      fields[2],
			Sub:         fields[3],
			Description: description,
		})
	}
	return units
}

func parseProperties(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	return props
}

func formatUnitDetails(props map[string]string) string {
	keys := []string{
		"Id",
		"Description",
		"LoadState",
		"ActiveState",
		"SubState",
		"UnitFileState",
		"FragmentPath",
		"Type",
		"Result",
		"ActiveEnterTimestamp",
		"ActiveExitTimestamp",
	}
	return formatPropertiesForKeys(props, keys)
}

func formatProcDetails(props map[string]string, psOut string) string {
	keys := []string{
		"MainPID",
		"ControlPID",
		"ExecMainPID",
		"NRestarts",
		"TasksCurrent",
		"MemoryCurrent",
		"CPUUsageNSec",
		"ActiveEnterTimestamp",
	}
	content := formatPropertiesForKeys(props, keys)
	if props["MainPID"] == "" || props["MainPID"] == "0" {
		return content + "\n\nNo active main process."
	}
	if psOut == "" {
		return content + "\n\nNo ps data for main process."
	}
	return content + "\n\nPID PPID CPU MEM RSS ELAPSED STAT COMMAND ARGS\n" + psOut
}

func formatProperties(out string) string {
	props := parseProperties(out)
	return formatPropertiesForKeys(props, sortedPropertyKeys(props))
}

func formatPropertiesForKeys(props map[string]string, keys []string) string {
	var b strings.Builder
	for _, key := range keys {
		value, ok := props[key]
		if !ok {
			continue
		}
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(&b, "%-22s %s\n", key, value)
	}
	return strings.TrimRight(b.String(), "\n")
}

func sortedPropertyKeys(props map[string]string) []string {
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func unitHeader(width int) string {
	return fitLine("  "+unitColumns("UNIT", "LOAD", "ACTIVE", "SUB", width-2), width)
}

func unitLine(unit unit, width int, selected bool) string {
	prefix := " "
	if selected {
		prefix = ">"
	}
	return fitLine(prefix+" "+unitColumns(unit.Name, unit.Load, unit.Active, unit.Sub, width-2), width)
}

func unitColumns(name string, load string, active string, sub string, width int) string {
	const (
		loadWidth          = 8
		activeWidth        = 8
		subWidth           = 14
		columnSpacing      = 2
		minNameWidth       = 10
		preferredNameWidth = 48
	)

	fixed := loadWidth + activeWidth + subWidth + 3*columnSpacing
	nameWidth := min(preferredNameWidth, width-fixed)
	if nameWidth < minNameWidth {
		return truncate(name, width)
	}

	return strings.Join([]string{
		padRight(truncate(name, nameWidth), nameWidth),
		padRight(truncate(load, loadWidth), loadWidth),
		padRight(truncate(active, activeWidth), activeWidth),
		padRight(truncate(sub, subWidth), subWidth),
	}, strings.Repeat(" ", columnSpacing))
}

func visibleLines(value string, offset int, height int) string {
	if height <= 0 {
		return ""
	}

	lines := strings.Split(value, "\n")
	offset = clamp(offset, 0, max(0, len(lines)-1))
	end := min(offset+height, len(lines))
	return strings.Join(lines[offset:end], "\n")
}

func clampScrollOffset(offset int, value string, height int) int {
	lines := strings.Split(value, "\n")
	return clamp(offset, 0, max(0, len(lines)-height))
}

func unitPageSize(height int) int {
	return max(1, height-1)
}

func emptyIfBlank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func modeName(userMode bool) string {
	if userMode {
		return "user"
	}
	return "system"
}

func paneIndex(viewName string) int {
	for i, pane := range panes {
		if pane == viewName {
			return i
		}
	}
	return 0
}

func paneLabel(viewName string) string {
	switch viewName {
	case viewUnits:
		return "units"
	case viewDetail:
		return "unit details"
	case viewProc:
		return "proc details"
	case viewLogs:
		return "journal"
	default:
		return viewName
	}
}

func fitLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(value) < width {
		return value
	}
	return truncate(value, width)
}

func padRight(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func clamp(value int, low int, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func init() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)
}
