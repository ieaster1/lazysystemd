# lazysystemd

A terminal UI for inspecting and controlling systemd units, using lazygit's
vendored `gocui` UI layer as the terminal foundation.

## Status

This is an initial scaffold. It can list system units, inspect the selected
unit, show recent journal output, and run common `systemctl` actions.

## Usage

```sh
go run .
```

Keys:

- `j` / `k` or arrow keys: move selection
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
