# lazysystemd

A terminal UI for inspecting and controlling systemd units, using lazygit's
vendored `gocui` UI layer as the terminal foundation.

## Status

This is an initial scaffold. It has four stable panes: unit inventory, system
diagnostics, selected-unit information, and debugging detail.

## Usage

```sh
go run .
```

Keys:

- `h` / `l` or left/right arrows: switch panes
- `Tab` / `Shift-Tab`: switch panes
- `1`-`4`: jump to a pane
- `j` / `k` or up/down arrows: move in the inventory pane, scroll in other panes
- `PgUp` / `PgDn`: scroll focused detail pane faster
- `[` / `]`: cycle tabs in the active pane
- `/`: filter units
- `Enter` / `Esc`: leave filter mode
- `r`: refresh units
- `u`: toggle system/user mode
- `s`: start selected unit
- `x`: stop selected unit
- `R`: restart selected unit
- `e`: enable selected unit
- `d`: disable selected unit
- `m`: mask selected unit
- `M`: unmask selected unit
- `q`: quit

Some actions against system units may require root privileges or a working
polkit agent, depending on the host.
