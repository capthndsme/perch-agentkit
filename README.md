# perch-agentkit

Shared Go code of the Perch device daemons: the
[Perch Network Collector](https://github.com/capthndsme/perch-collector) and the
[Perch AP Daemon](https://github.com/capthndsme/perch-apd). Both dial the
[Perch Network Controller](https://github.com/capthndsme/perch-controller) over a
WebSocket, speak JSON-RPC 2.0 on it and push on a schedule the controller sets,
so that part lives here once.

| Package | What |
|---|---|
| `rpc` | JSON-RPC 2.0, one object per WebSocket message: codec, dispatcher, requests, notifications |
| `link` | One session to the controller: dial (TLS options, subprotocol, optional permessage-deflate), pings, serving the controller's requests, routing its notifications, calls of our own, the push scheduler driven by `agent.configure`, reconnect backoff, the controller's HTTP error bodies |
| `hoststat` | Typed readers for `/proc`: load, memory, interface counters, conntrack fill, SNMP counters, default-route interfaces; and the host's Ethernet ports from `/sys/class/net` and `/etc/board.json`. All over a root that tests can point at a fixture tree |

Reconnect policy is the daemon's: `link.Run` is one session and returns why it
ended (a `*link.StatusError` for a refused upgrade, the close code otherwise).

## Ports

`hoststat.PortReader` lists the host's Ethernet ports with their live link state,
for the controller's infrastructure view: the JSON array both daemons send
(`docs/infrastructure-view.md` §2.1 and §4 in perch-controller). It tells a port from
everything else in `/sys/class/net`: DSA user ports count and the DSA conduit does not;
per-port netdevs and a second MAC used as WAN count; an LTE modem in 802.3 mode
(`DEVTYPE=wwan`) counts with medium `wireless`; bridges, bonds, teams, VLANs on a local
device, Wi-Fi interfaces, tunnels, ifb, dummy and tun/tap devices do not. A veth, macvlan
or VLAN whose other end or parent is in another network namespace counts as a *virtual*
port, by default only on a host that has no wired hardware port (a router in a
container; an LTE modem next to its veths does not hide them).
`role` comes from `board.json` (OpenWrt; hardware ports only, since a container sees
its host's file) plus the WAN interfaces the caller names, and the result is in display
order: WAN first, then the board's LAN order, then the rest in natural order.

```go
r := &hoststat.PortReader{Options: hoststat.PortOptions{WAN: []string{"wan0"}}}
ports := r.Read() // nil: /sys/class/net unreadable; []: looked, found none
```

A reader keeps what does not change (classification, label, medium, MAC) per
name + ifindex for five minutes and `board.json` as long, so a read in steady state is
one directory listing plus an open/read/close of about seven small attributes per port.
Link state (`adminUp`, `carrier`, `operstate`, `speedMbps`, `duplex`, `carrierChanges`)
is read fresh every time; what the kernel does not answer (carrier of a port that is
administratively down, speed -1 without a link) is left out. `FS.Ports` is the one-shot
form. The fixture trees in `hoststat/testdata/ports/` are named after the port layouts
they model (DSA with a conduit, per-port netdevs, a second MAC as WAN, a container with
only veths, a VM, and a tree of everything that is not a port).

Go 1.22 or newer (the OpenWrt SDK floor); one dependency,
`github.com/coder/websocket`. No cgo.

```
go test ./...
GOTOOLCHAIN=go1.22.12 go test ./...
```

## License

MIT; see [LICENSE](LICENSE).
