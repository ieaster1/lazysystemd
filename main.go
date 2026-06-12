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
	roundedBox   = []rune{'─', '│', '╭', '╮', '╰', '╯', '├', '┤', '┬', '┴', '┼'}
	unitKindTabs = []unitKind{
		{label: "Units", systemctlTypes: []string{"service", "timer"}},
		{label: "Sockets", systemctlTypes: []string{"socket"}},
		{label: "Mounts", systemctlTypes: []string{"mount"}},
	}
	systemTabLabels = []string{"Blame", "Analyze", "Failed"}
	infoTabLabels   = []string{"Details", "Stats"}
	debugTabLabels  = []string{"Journal", "Dependencies"}
	actions         = []unitActionBinding{
		{key: 's', action: "start"},
		{key: 'x', action: "stop"},
		{key: 'R', action: "restart"},
		{key: 'e', action: "enable"},
		{key: 'd', action: "disable"},
		{key: 'm', action: "mask"},
		{key: 'M', action: "unmask"},
	}
)

type paneSpec struct {
	viewName string
	label    func(*app) string
	help     func(*app) string
	cycle    func(*app, int)
	scroll   func(*app, int)
	jumpTop  func(*app)
	jumpEnd  func(*app)
	render   func(*app, *gocui.View)
}

type unitKind struct {
	label          string
	systemctlTypes []string
}

type tabSet[T any] struct {
	tabs  []T
	index int
}

func newTabSet[T any](tabs []T) tabSet[T] {
	return tabSet[T]{tabs: tabs}
}

func (t tabSet[T]) current() T {
	return t.tabs[t.index]
}

func (t *tabSet[T]) cycle(delta int) {
	t.index = cycleIndex(t.index, len(t.tabs), delta)
}

func (t tabSet[T]) label(labeler func(T) string) string {
	return labeler(t.current())
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

func (r runner) systemdAnalyze(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemd-analyze", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

type app struct {
	g      *gocui.Gui
	mu     sync.Mutex
	runner runner

	units          []unit
	selected       int
	unitOffset     int
	systemOffset   int
	procOffset     int
	logOffset      int
	filter         string
	filtering      bool
	pendingG       bool
	focusedPane    string
	unitTabs       tabSet[unitKind]
	systemTabs     tabSet[string]
	infoTabs       tabSet[string]
	debugTabs      tabSet[string]
	status         string
	system         string
	detail         string
	proc           string
	logs           string
	dependencies   string
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
		unitTabs:    newTabSet(unitKindTabs),
		systemTabs:  newTabSet(systemTabLabels),
		infoTabs:    newTabSet(infoTabLabels),
		debugTabs:   newTabSet(debugTabLabels),
		status:      "Loading units...",
	}

	g.Highlight = true
	g.SelFgColor = gocui.ColorGreen
	g.SelFrameColor = gocui.ColorGreen
	g.ErrorHandler = func(err error) error {
		if errors.Is(err, gocui.ErrKeybindingNotHandled) {
			return nil
		}
		return err
	}
	g.SetManagerFunc(a.layout)
	a.bindKeys()
	a.refresh()

	err = g.MainLoop()
	if errors.Is(err, gocui.ErrQuit) {
		return nil
	}
	return err
}

func (a *app) paneSpecs() []paneSpec {
	return []paneSpec{
		{
			viewName: viewUnits,
			label:    func(a *app) string { return a.unitTabs.label(func(kind unitKind) string { return kind.label }) },
			help:     func(*app) string { return "j/k move | [/]/ tab | / filter | s/x/R service" },
			cycle:    func(a *app, delta int) { a.cycleUnitTab(delta) },
			scroll:   func(a *app, delta int) { a.moveSelection(delta) },
			jumpTop:  func(a *app) { a.jumpUnitsTop() },
			jumpEnd:  func(a *app) { a.jumpUnitsBottom() },
			render:   func(a *app, view *gocui.View) { a.renderUnitsPane(view) },
		},
		{
			viewName: viewDetail,
			label:    func(a *app) string { return a.systemTabs.current() },
			help:     func(*app) string { return "j/k scroll | [/]/ tab | system diagnostics" },
			cycle:    func(a *app, delta int) { a.cycleSystemTab(delta) },
			scroll:   func(a *app, delta int) { a.scrollTextPane(&a.systemOffset, a.systemContent(), delta) },
			jumpTop:  func(a *app) { a.jumpTextPaneTop(&a.systemOffset) },
			jumpEnd:  func(a *app) { a.jumpTextPaneEnd(&a.systemOffset, a.systemContent()) },
			render: func(a *app, view *gocui.View) {
				a.renderTextPane(view, a.systemContent(), &a.systemOffset, "No system diagnostics loaded.")
			},
		},
		{
			viewName: viewProc,
			label:    func(a *app) string { return a.infoTabs.current() },
			help:     func(*app) string { return "j/k scroll | [/]/ tab | selected unit info" },
			cycle:    func(a *app, delta int) { a.cycleInfoTab(delta) },
			scroll:   func(a *app, delta int) { a.scrollTextPane(&a.procOffset, a.infoContent(), delta) },
			jumpTop:  func(a *app) { a.jumpTextPaneTop(&a.procOffset) },
			jumpEnd:  func(a *app) { a.jumpTextPaneEnd(&a.procOffset, a.infoContent()) },
			render: func(a *app, view *gocui.View) {
				a.renderTextPane(view, a.infoContent(), &a.procOffset, "No unit selected.")
			},
		},
		{
			viewName: viewLogs,
			label:    func(a *app) string { return a.debugTabs.current() },
			help:     func(*app) string { return "j/k scroll | [/]/ tab | debug details" },
			cycle:    func(a *app, delta int) { a.cycleDebugTab(delta) },
			scroll:   func(a *app, delta int) { a.scrollTextPane(&a.logOffset, a.debugContent(), delta) },
			jumpTop:  func(a *app) { a.jumpTextPaneTop(&a.logOffset) },
			jumpEnd:  func(a *app) { a.jumpTextPaneEnd(&a.logOffset, a.debugContent()) },
			render: func(a *app, view *gocui.View) {
				a.renderTextPane(view, a.debugContent(), &a.logOffset, "No debug details loaded.")
			},
		},
	}
}

func (a *app) focusedPaneSpec() paneSpec {
	return a.paneSpec(a.focusedPane)
}

func (a *app) paneSpec(viewName string) paneSpec {
	for _, pane := range a.paneSpecs() {
		if pane.viewName == viewName {
			return pane
		}
	}
	return a.paneSpecs()[0]
}

func (a *app) focusedPaneIndex() int {
	for i, pane := range a.paneSpecs() {
		if pane.viewName == a.focusedPane {
			return i
		}
	}
	return 0
}

func (a *app) cycleUnitTab(delta int) {
	a.mu.Lock()
	a.unitTabs.cycle(delta)
	a.selected = 0
	a.unitOffset = 0
	a.procOffset = 0
	a.logOffset = 0
	a.detail = ""
	a.proc = ""
	a.logs = ""
	a.dependencies = ""
	a.status = "Switched to " + a.currentKind().label + "."
	a.mu.Unlock()
	a.refresh()
}

func (a *app) cycleSystemTab(delta int) {
	a.mu.Lock()
	a.systemTabs.cycle(delta)
	a.systemOffset = 0
	a.status = "Switched to " + a.currentSystemTab() + "."
	a.mu.Unlock()
	a.loadSystem()
}

func (a *app) cycleInfoTab(delta int) {
	a.mu.Lock()
	a.infoTabs.cycle(delta)
	a.procOffset = 0
	a.status = "Switched to " + a.currentInfoTab() + "."
	a.mu.Unlock()
	a.render()
}

func (a *app) cycleDebugTab(delta int) {
	a.mu.Lock()
	a.debugTabs.cycle(delta)
	a.logOffset = 0
	a.status = "Switched to " + a.currentDebugTab() + "."
	a.mu.Unlock()
	a.render()
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
		a.clearPendingG()
		return nil
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

	for i, pane := range a.paneSpecs() {
		i, pane := i, pane
		a.g.SetKeybinding("", gocui.NewKeyRune(rune('1'+i)), func(*gocui.Gui, *gocui.View) error {
			if a.filteringRune(rune('1' + i)) {
				return nil
			}
			a.focusPane(pane.viewName)
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
		a.cycleActiveTab(-1)
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune(']'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune(']') {
			return nil
		}
		a.cycleActiveTab(1)
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
		a.clearPendingG()
		a.refresh()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('u'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('u') {
			return nil
		}
		a.clearPendingG()
		a.toggleUserMode()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('g'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('g') {
			return nil
		}
		a.handleG()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('G'), func(*gocui.Gui, *gocui.View) error {
		if a.filteringRune('G') {
			return nil
		}
		a.jumpBottom()
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
			a.clearPendingG()
			return nil
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

	leftSplit := bodyBottom / 2
	rightSplit := bodyBottom / 2

	units, err := g.SetView(viewUnits, 0, 0, leftW-1, leftSplit, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	system, err := g.SetView(viewDetail, 0, leftSplit+1, leftW-1, bodyBottom, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	info, err := g.SetView(viewProc, leftW, 0, maxX-1, rightSplit, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	debug, err := g.SetView(viewLogs, leftW, rightSplit+1, maxX-1, bodyBottom, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	status, err := g.SetView(viewStatus, 0, bodyBottom+1, maxX-1, maxY-1, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, view := range []*gocui.View{units, system, info, debug} {
		view.FrameRunes = roundedBox
		view.Highlight = false
		view.Footer = ""
	}
	status.Frame = false

	viewsByName := map[string]*gocui.View{
		viewUnits:  units,
		viewDetail: system,
		viewProc:   info,
		viewLogs:   debug,
	}
	for _, pane := range a.paneSpecs() {
		view := viewsByName[pane.viewName]
		view.Title = a.titleFor(pane.viewName, pane.label(a))
	}

	filtered := a.filteredUnits()
	a.clampSelection(len(filtered))
	a.keepSelectionVisible(units.InnerHeight())

	for _, pane := range a.paneSpecs() {
		pane.render(a, viewsByName[pane.viewName])
	}
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
	a.loadSystem()

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

func (a *app) loadSystem() {
	tab := a.currentSystemTab()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var out string
		var err error
		switch tab {
		case "Blame":
			out, err = a.runner.systemdAnalyze(ctx, "blame")
		case "Analyze":
			out, err = a.runner.systemdAnalyze(ctx, "critical-chain")
		case "Failed":
			out, err = a.runner.systemctl(ctx, "list-units", "--failed", "--no-pager", "--plain")
		}

		a.mu.Lock()
		if err != nil {
			a.system = strings.TrimSpace(out)
		} else {
			a.system = out
		}
		a.systemOffset = 0
		a.mu.Unlock()
		a.render()
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
		a.dependencies = ""
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
		dependencies, dependenciesErr := a.runner.systemctl(ctx, "list-dependencies", unitName, "--no-pager", "--plain")
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
		if dependenciesErr != nil {
			a.dependencies = fmt.Sprintf("systemctl list-dependencies failed:\n%s", dependencies)
		} else {
			a.dependencies = dependencies
		}
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
	pane := a.focusedPaneSpec()
	a.pendingG = false
	a.mu.Unlock()

	pane.scroll(a, delta)
	return nil
}

func (a *app) handleG() {
	a.mu.Lock()
	if a.pendingG {
		a.pendingG = false
		a.mu.Unlock()
		a.jumpTop()
		return
	}
	a.pendingG = true
	a.status = "g"
	a.mu.Unlock()
	a.render()
}

func (a *app) clearPendingG() {
	a.mu.Lock()
	a.pendingG = false
	a.mu.Unlock()
}

func (a *app) jumpTop() {
	a.mu.Lock()
	pane := a.focusedPaneSpec()
	a.mu.Unlock()
	pane.jumpTop(a)
}

func (a *app) jumpBottom() {
	a.mu.Lock()
	a.pendingG = false
	pane := a.focusedPaneSpec()
	a.mu.Unlock()
	pane.jumpEnd(a)
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

func (a *app) jumpUnitsTop() {
	a.mu.Lock()
	a.selected = 0
	a.unitOffset = 0
	a.status = "Jumped to top."
	a.mu.Unlock()
	a.loadSelected()
	a.render()
}

func (a *app) jumpUnitsBottom() {
	a.mu.Lock()
	filtered := a.filteredUnits()
	if len(filtered) > 0 {
		a.selected = len(filtered) - 1
		a.unitOffset = a.selected
	}
	a.status = "Jumped to bottom."
	a.mu.Unlock()
	a.loadSelected()
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
	a.pendingG = false
	a.focusedPane = pane
	a.status = fmt.Sprintf("Focused %s.", a.focusedPaneSpec().label(a))
	a.mu.Unlock()
	a.render()
}

func (a *app) moveFocus(delta int) {
	a.mu.Lock()
	a.pendingG = false
	panes := a.paneSpecs()
	idx := a.focusedPaneIndex() + delta
	idx = clamp(idx, 0, len(panes)-1)
	a.focusedPane = panes[idx].viewName
	a.status = fmt.Sprintf("Focused %s.", panes[idx].label(a))
	a.mu.Unlock()
	a.render()
}

func (a *app) cycleActiveTab(delta int) {
	a.mu.Lock()
	a.pendingG = false
	pane := a.focusedPaneSpec()
	a.mu.Unlock()
	pane.cycle(a, delta)
}

func (a *app) toggleUserMode() {
	a.mu.Lock()
	a.pendingG = false
	a.runner.userMode = !a.runner.userMode
	mode := modeName(a.runner.userMode)
	a.status = "Switched to " + mode + " units."
	a.selected = 0
	a.unitOffset = 0
	a.systemOffset = 0
	a.procOffset = 0
	a.logOffset = 0
	a.system = ""
	a.detail = ""
	a.proc = ""
	a.logs = ""
	a.dependencies = ""
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
		a.pendingG = false
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

func (a *app) renderUnitsPane(view *gocui.View) {
	filtered := a.filteredUnits()
	a.clampSelection(len(filtered))
	a.keepSelectionVisible(view.InnerHeight())
	view.SetContent(a.unitsContent(filtered, view.InnerWidth(), view.InnerHeight()))
	if len(filtered) > 0 {
		view.SetHighlight(a.selected-a.unitOffset+1, true)
	}
}

func (a *app) renderTextPane(view *gocui.View, content string, offset *int, emptyMessage string) {
	*offset = clampScrollOffset(*offset, content, view.InnerHeight())
	view.SetContent(visibleLines(emptyIfBlank(content, emptyMessage), *offset, view.InnerHeight()))
}

func (a *app) scrollTextPane(offset *int, content string, delta int) {
	a.mu.Lock()
	*offset = clampScrollOffset(*offset+delta, content, 1)
	a.mu.Unlock()
	a.render()
}

func (a *app) jumpTextPaneTop(offset *int) {
	a.mu.Lock()
	*offset = 0
	a.status = "Jumped to top."
	a.mu.Unlock()
	a.render()
}

func (a *app) jumpTextPaneEnd(offset *int, content string) {
	a.mu.Lock()
	*offset = clampScrollOffset(1<<30, content, 1)
	a.status = "Jumped to bottom."
	a.mu.Unlock()
	a.render()
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
	a.pendingG = false
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
	return a.unitTabs.current()
}

func (a *app) currentSystemTab() string {
	return a.systemTabs.current()
}

func (a *app) currentInfoTab() string {
	return a.infoTabs.current()
}

func (a *app) currentDebugTab() string {
	return a.debugTabs.current()
}

func (a *app) systemContent() string {
	return a.system
}

func (a *app) infoContent() string {
	switch a.currentInfoTab() {
	case "Details":
		return a.detail
	case "Stats":
		return a.proc
	default:
		return ""
	}
}

func (a *app) debugContent() string {
	switch a.currentDebugTab() {
	case "Journal":
		return a.logs
	case "Dependencies":
		return a.dependencies
	default:
		return ""
	}
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

	return a.focusedPaneSpec().help(a)
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

func cycleIndex(index int, length int, delta int) int {
	return (index + delta + length) % length
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
