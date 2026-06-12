package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseUnits(t *testing.T) {
	output := `
sshd.service loaded active running OpenSSH server daemon
systemd-tmpfiles-clean.timer loaded active waiting Daily Cleanup of Temporary Directories
broken.service loaded failed failed Broken example service
`

	assert.Equal(t, []unit{
		{
			Name:        "sshd.service",
			Load:        "loaded",
			Active:      "active",
			Sub:         "running",
			Description: "OpenSSH server daemon",
		},
		{
			Name:        "systemd-tmpfiles-clean.timer",
			Load:        "loaded",
			Active:      "active",
			Sub:         "waiting",
			Description: "Daily Cleanup of Temporary Directories",
		},
		{
			Name:        "broken.service",
			Load:        "loaded",
			Active:      "failed",
			Sub:         "failed",
			Description: "Broken example service",
		},
	}, parseUnits(output))
}

func TestFormatProperties(t *testing.T) {
	output := "Id=sshd.service\nActiveState=active\nFragmentPath=\n"

	assert.Equal(t, "ActiveState            active\nFragmentPath           -\nId                     sshd.service", formatProperties(output))
}

func TestFilteredUnitsMatchesKeyFields(t *testing.T) {
	app := &app{
		filter: "waiting",
		units: []unit{
			{Name: "sshd.service", Active: "active", Sub: "running", Description: "OpenSSH server daemon"},
			{Name: "systemd-tmpfiles-clean.timer", Active: "active", Sub: "waiting", Description: "Daily Cleanup of Temporary Directories"},
		},
	}

	assert.Equal(t, []unit{
		{Name: "systemd-tmpfiles-clean.timer", Active: "active", Sub: "waiting", Description: "Daily Cleanup of Temporary Directories"},
	}, app.filteredUnits())
}

func TestKeepSelectionVisibleScrollsUnits(t *testing.T) {
	app := &app{
		selected:   8,
		unitOffset: 0,
	}

	app.keepSelectionVisible(5)

	assert.Equal(t, 5, app.unitOffset)
}

func TestUnitLinePreservesLongSubstateWithoutDescription(t *testing.T) {
	line := unitLine(unit{
		Name:        "example.service",
		Load:        "loaded",
		Active:      "activating",
		Sub:         "auto-restart",
		Description: "Example Service",
	}, 80, true)

	assert.Contains(t, line, "auto-restart")
	assert.NotContains(t, line, "Example Service")
	assert.LessOrEqual(t, len(line), 80)
}

func TestParseProperties(t *testing.T) {
	assert.Equal(t, map[string]string{
		"Id":          "sshd.service",
		"ActiveState": "active",
	}, parseProperties("Id=sshd.service\nActiveState=active\n"))
}

func TestFormatProcDetailsShowsNoProcess(t *testing.T) {
	output := formatProcDetails(map[string]string{
		"MainPID":   "0",
		"NRestarts": "2",
	}, "")

	assert.Contains(t, output, "MainPID")
	assert.Contains(t, output, "No active main process.")
}
