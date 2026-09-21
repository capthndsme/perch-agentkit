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
| `hoststat` | Typed readers for `/proc`: load, memory, interface counters, conntrack fill, SNMP counters, default-route interfaces, over a root that tests can point at a fixture tree |

Reconnect policy is the daemon's: `link.Run` is one session and returns why it
ended (a `*link.StatusError` for a refused upgrade, the close code otherwise).

Go 1.22 or newer (the OpenWrt SDK floor); one dependency,
`github.com/coder/websocket`. No cgo.

```
go test ./...
GOTOOLCHAIN=go1.22.12 go test ./...
```

## License

MIT; see [LICENSE](LICENSE).
