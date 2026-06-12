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
	viewLogs   = "logs"
	viewStatus = "status"
)

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

type app struct {
	g      *gocui.Gui
	mu     sync.Mutex
	runner runner

	units    []unit
	selected int
	status   string
	detail   string
	logs     string
	loading  bool
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
		g:      g,
		status: "Loading units...",
	}

	g.Highlight = true
	g.SelFgColor = gocui.ColorGreen
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
	quit := func(*gocui.Gui, *gocui.View) error { return gocui.ErrQuit }
	a.g.SetKeybinding("", gocui.NewKeyRune('q'), quit)
	a.g.SetKeybinding("", gocui.NewKeyStrMod("c", gocui.ModCtrl), quit)
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyEsc), quit)

	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyArrowUp), func(*gocui.Gui, *gocui.View) error {
		a.moveSelection(-1)
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('k'), func(*gocui.Gui, *gocui.View) error {
		a.moveSelection(-1)
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyName(gocui.KeyArrowDown), func(*gocui.Gui, *gocui.View) error {
		a.moveSelection(1)
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('j'), func(*gocui.Gui, *gocui.View) error {
		a.moveSelection(1)
		return nil
	})

	a.g.SetKeybinding("", gocui.NewKeyRune('r'), func(*gocui.Gui, *gocui.View) error {
		a.refresh()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('u'), func(*gocui.Gui, *gocui.View) error {
		a.toggleUserMode()
		return nil
	})
	a.g.SetKeybinding("", gocui.NewKeyRune('s'), a.unitAction("start"))
	a.g.SetKeybinding("", gocui.NewKeyRune('x'), a.unitAction("stop"))
	a.g.SetKeybinding("", gocui.NewKeyRune('R'), a.unitAction("restart"))
	a.g.SetKeybinding("", gocui.NewKeyRune('e'), a.unitAction("enable"))
	a.g.SetKeybinding("", gocui.NewKeyRune('d'), a.unitAction("disable"))
	a.g.SetKeybinding("", gocui.NewKeyRune('m'), a.unitAction("mask"))
	a.g.SetKeybinding("", gocui.NewKeyRune('M'), a.unitAction("unmask"))
}

func (a *app) layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()
	if maxX < 40 || maxY < 12 {
		return nil
	}

	leftW := maxX / 3
	if leftW < 34 {
		leftW = 34
	}
	if leftW > maxX-28 {
		leftW = maxX - 28
	}

	units, err := g.SetView(viewUnits, 0, 0, leftW-1, maxY-3, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	detail, err := g.SetView(viewDetail, leftW, 0, maxX-1, maxY/2-1, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	logs, err := g.SetView(viewLogs, leftW, maxY/2, maxX-1, maxY-3, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	status, err := g.SetView(viewStatus, 0, maxY-2, maxX-1, maxY-1, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}

	if _, err := g.SetCurrentView(viewUnits); err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	units.Title = " Units "
	detail.Title = " Details "
	logs.Title = " Journal "
	status.Frame = false

	units.SetContent(a.unitsContent())
	detail.SetContent(emptyIfBlank(a.detail, "No unit selected."))
	logs.SetContent(emptyIfBlank(a.logs, "No journal entries loaded."))
	status.SetContent(a.statusLine())

	return nil
}

func (a *app) refresh() {
	a.mu.Lock()
	a.loading = true
	a.status = "Loading units..."
	a.mu.Unlock()
	a.render()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		out, err := a.runner.systemctl(ctx,
			"list-units",
			"--all",
			"--type", "service",
			"--type", "timer",
			"--type", "socket",
			"--type", "mount",
			"--no-legend",
			"--no-pager",
			"--plain",
		)

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
			if a.selected >= len(a.units) {
				a.selected = len(a.units) - 1
			}
			if a.selected < 0 {
				a.selected = 0
			}
			a.status = fmt.Sprintf("Loaded %d units.", len(a.units))
		}
		a.mu.Unlock()

		a.loadSelected()
	}()
}

func (a *app) loadSelected() {
	unitName := a.selectedUnitName()
	if unitName == "" {
		a.mu.Lock()
		a.detail = ""
		a.logs = ""
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
			"--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState,FragmentPath,MainPID,ExecMainPID,Type,Result,ActiveEnterTimestamp,ActiveExitTimestamp",
		)
		logs, logsErr := a.runner.journalctl(ctx, unitName)

		a.mu.Lock()
		if detailErr != nil {
			a.detail = fmt.Sprintf("systemctl show failed:\n%s", detail)
		} else {
			a.detail = formatProperties(detail)
		}
		if logsErr != nil {
			a.logs = fmt.Sprintf("journalctl failed:\n%s", logs)
		} else {
			a.logs = logs
		}
		a.mu.Unlock()
		a.render()
	}()
}

func (a *app) moveSelection(delta int) {
	a.mu.Lock()
	if len(a.units) == 0 {
		a.mu.Unlock()
		return
	}
	a.selected += delta
	if a.selected < 0 {
		a.selected = 0
	}
	if a.selected >= len(a.units) {
		a.selected = len(a.units) - 1
	}
	a.mu.Unlock()
	a.loadSelected()
	a.render()
}

func (a *app) toggleUserMode() {
	a.mu.Lock()
	a.runner.userMode = !a.runner.userMode
	mode := modeName(a.runner.userMode)
	a.status = "Switched to " + mode + " units."
	a.selected = 0
	a.detail = ""
	a.logs = ""
	a.mu.Unlock()
	a.refresh()
}

func (a *app) unitAction(action string) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
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
	if a.selected < 0 || a.selected >= len(a.units) {
		return ""
	}
	return a.units[a.selected].Name
}

func (a *app) unitsContent() string {
	if len(a.units) == 0 {
		if a.loading {
			return "Loading..."
		}
		return "No units found."
	}

	var b strings.Builder
	for i, u := range a.units {
		prefix := " "
		if i == a.selected {
			prefix = ">"
		}
		fmt.Fprintf(&b, "%s %-42s %-8s %-10s %s\n", prefix, truncate(u.Name, 42), u.Active, u.Sub, u.Description)
	}
	return b.String()
}

func (a *app) statusLine() string {
	mode := modeName(a.runner.userMode)
	return fmt.Sprintf(" %s | j/k up/down | r refresh | u mode | s start | x stop | R restart | e enable | d disable | m/M mask/unmask | q quit | %s",
		mode,
		a.status,
	)
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

func formatProperties(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
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

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func init() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)
}
