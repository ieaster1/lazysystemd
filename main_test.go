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

	assert.Equal(t, "Id                     sshd.service\nActiveState            active\nFragmentPath           -", formatProperties(output))
}
