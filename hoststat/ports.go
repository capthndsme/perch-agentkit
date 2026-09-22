package hoststat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Port is one Ethernet port of the host as the kernel describes it
// (docs/infrastructure-view.md §2.1 in perch-controller). Fields the kernel
// does not answer for are left out rather than guessed.
type Port struct {
	Name           string  `json:"name"`
	Label          string  `json:"label,omitempty"`
	Role           string  `json:"role,omitempty"`   // "wan" | "lan"
	Medium         string  `json:"medium,omitempty"` // "copper" | "sfp" | "virtual" | "wireless"
	MAC            string  `json:"mac,omitempty"`
	AdminUp        *bool   `json:"adminUp,omitempty"`
	Carrier        *bool   `json:"carrier,omitempty"`
	Operstate      string  `json:"operstate,omitempty"`
	SpeedMbps      *int    `json:"speedMbps,omitempty"`
	Duplex         string  `json:"duplex,omitempty"` // "full" | "half"
	CarrierChanges *uint64 `json:"carrierChanges,omitempty"`
}

// VirtualPolicy decides whether NICs whose peer is in another network
// namespace count as ports.
type VirtualPolicy string

const (
	// VirtualAuto reports them only when the host has no wired hardware
	// port (an LTE modem does not count): a router in a container has
	// nothing else (§2).
	VirtualAuto   VirtualPolicy = ""
	VirtualNever  VirtualPolicy = "never"
	VirtualAlways VirtualPolicy = "always"
)

// BoardPorts is /etc/board.json's port roles, in board order.
type BoardPorts struct{ LAN, WAN []string }

// PortOptions tune what Read reports.
type PortOptions struct {
	// Virtual is the virtual NIC policy; any value other than VirtualNever
	// and VirtualAlways means VirtualAuto.
	Virtual VirtualPolicy
	// WAN marks extra interfaces as role "wan" (the collector passes the
	// interfaces its gateway stats count).
	WAN []string
	// Max caps the result (default 64).
	Max int
}

// PortReader reads ports repeatedly, caching the facts that do not change
// (device type, label, medium, MAC) for TTL (default 5 min) per name+ifindex,
// and board.json for as long, so a 5 s push costs one ReadDir and ~7 small
// reads per port.
//
// Read is safe for concurrent use. Options may be changed between reads (not
// during one): roles are worked out on every read from the cached board.json
// and the current Options, so a new WAN list applies at once.
type PortReader struct {
	FS      FS
	Options PortOptions
	TTL     time.Duration

	mu      sync.Mutex
	now     func() time.Time // tests
	gen     uint64
	facts   map[string]*portFacts // by netdev name; only names the last read saw
	board   BoardPorts
	boardAt time.Time
	boardOK bool
	buf     [64]byte
}

const (
	sysClassNet    = "/sys/class/net"
	defaultPortTTL = 5 * time.Minute
	defaultPortMax = 64

	iffUp = 0x1 // IFF_UP in net_device.flags: administratively up
)

type portKind uint8

const (
	notAPort portKind = iota
	hardwarePort
	virtualNIC
)

// portFacts is what the reader keeps about a netdev between reads.
type portFacts struct {
	name    string
	ifindex string
	kind    portKind
	wired   bool // a hardware port other than a cellular modem (VirtualAuto)
	label   string
	medium  string
	mac     string
	at      time.Time // when the facts were read
	gen     uint64    // the last read that saw the netdev
}

// Ports is the one-shot form (CLI, tests).
func (f FS) Ports(o PortOptions) []Port {
	r := PortReader{FS: f, Options: o}
	return r.Read()
}

// Read lists the host's ports in display order: role "wan" first, then
// board.json's LAN order, then the rest in natural order ("lan2" before
// "lan10"), capped at Options.Max. board.json roles apply to hardware ports
// only; Options.WAN to every port. It never fails: a file it cannot read
// leaves its field out. The result is nil only when /sys/class/net itself
// cannot be listed; a host without ports gets a non-nil, empty slice, so a
// caller can tell "could not look" from "looked and found none".
func (r *PortReader) Read() []Port {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = defaultPortTTL
	}
	fresh := func(at time.Time) bool {
		age := now.Sub(at)
		return age >= 0 && age < ttl
	}

	entries, err := r.FS.ReadDir(sysClassNet)
	if err != nil {
		return nil
	}
	if !r.boardOK || !fresh(r.boardAt) {
		r.board, r.boardAt, r.boardOK = r.FS.BoardPorts(), now, true
	}
	if r.facts == nil {
		r.facts = make(map[string]*portFacts, len(entries))
	}
	r.gen++
	var hardware, virtual []*portFacts
	wired := false
	for _, e := range entries {
		// Rule 1: a directory (sysfs lists each netdev as a symlink to one;
		// bonding_masters is a regular file), and not lo.
		name := e.Name()
		if name == "lo" || e.Type().IsRegular() {
			continue
		}
		idx := r.FS.readAttr(r.buf[:], sysClassNet+"/"+name+"/ifindex")
		if len(idx) == 0 {
			continue // not a netdev directory, or gone since the listing
		}
		pf := r.facts[name]
		if pf == nil || pf.ifindex != string(idx) || !fresh(pf.at) {
			pf = r.FS.portFacts(name, string(idx))
			pf.at = now
			r.facts[name] = pf
		}
		pf.gen = r.gen
		switch pf.kind {
		case hardwarePort:
			hardware = append(hardware, pf)
			wired = wired || pf.wired
		case virtualNIC:
			virtual = append(virtual, pf)
		}
	}
	// The cache holds exactly the netdevs of this listing: churn (container
	// veths come and go under new names) cannot grow it.
	for name, pf := range r.facts {
		if pf.gen != r.gen {
			delete(r.facts, name)
		}
	}

	o := r.Options
	chosen := hardware
	switch o.Virtual {
	case VirtualAlways:
		chosen = append(hardware, virtual...)
	case VirtualNever:
	default:
		// Virtual NICs stand in for ports only where no wired port exists:
		// a container gateway keeps its veths next to a passed-through modem.
		if !wired {
			chosen = append(hardware, virtual...)
		}
	}

	type ranked struct {
		Port
		group, idx int
	}
	// Sorted through pointers: the daemons already carry that instantiation
	// of slices.SortFunc, where a struct element would add one (12 KB on MIPS).
	list := make([]*ranked, 0, len(chosen))
	for _, pf := range chosen {
		name := pf.name
		p := &ranked{Port: Port{Name: name, Label: pf.label, Medium: pf.medium, MAC: pf.mac}, group: 2}
		// board.json describes the hardware: in a container it is the host's
		// and may name a veth (A2). The caller's WAN list applies to all.
		boardWAN, boardLAN := -1, -1
		if pf.kind == hardwarePort {
			boardWAN, boardLAN = slices.Index(r.board.WAN, name), slices.Index(r.board.LAN, name)
		}
		switch i := slices.Index(o.WAN, name); {
		case boardWAN >= 0:
			p.Role, p.group, p.idx = "wan", 0, boardWAN
		case i >= 0:
			p.Role, p.group, p.idx = "wan", 0, len(r.board.WAN)+i
		case boardLAN >= 0:
			p.Role, p.group, p.idx = "lan", 1, boardLAN
		}
		list = append(list, p)
	}
	slices.SortFunc(list, func(a, b *ranked) int {
		switch {
		case a.group != b.group:
			return a.group - b.group
		case a.idx != b.idx:
			return a.idx - b.idx
		}
		return compareNatural(a.Name, b.Name)
	})
	max := o.Max
	if max <= 0 {
		max = defaultPortMax
	}
	if len(list) > max {
		list = list[:max]
	}
	out := make([]Port, len(list))
	for i := range list {
		out[i] = list[i].Port
		r.readState(&out[i])
	}
	return out
}

// readState fills the link state, which is read fresh on every Read.
func (r *PortReader) readState(p *Port) {
	dir := sysClassNet + "/" + p.Name + "/"
	if b := r.FS.readAttr(r.buf[:], dir+"flags"); b != nil {
		if v, err := strconv.ParseUint(strings.TrimPrefix(string(b), "0x"), 16, 32); err == nil {
			up := v&iffUp != 0
			p.AdminUp = &up
		}
	}
	// carrier, speed and duplex fail with EINVAL while the port is
	// administratively down; speed is -1 and duplex "unknown" without a link.
	if b := r.FS.readAttr(r.buf[:], dir+"carrier"); len(b) == 1 && (b[0] == '0' || b[0] == '1') {
		c := b[0] == '1'
		p.Carrier = &c
	}
	if b := r.FS.readAttr(r.buf[:], dir+"operstate"); b != nil {
		p.Operstate = operstate(b)
	}
	if b := r.FS.readAttr(r.buf[:], dir+"speed"); b != nil {
		if v, err := strconv.Atoi(string(b)); err == nil && v > 0 {
			p.SpeedMbps = &v
		}
	}
	if b := r.FS.readAttr(r.buf[:], dir+"duplex"); b != nil {
		switch string(b) {
		case "full":
			p.Duplex = "full"
		case "half":
			p.Duplex = "half"
		}
	}
	if b := r.FS.readAttr(r.buf[:], dir+"carrier_changes"); b != nil {
		if v, err := strconv.ParseUint(string(b), 10, 64); err == nil {
			p.CarrierChanges = &v
		}
	}
}

// operstate returns the kernel's operstate word, without allocating for the
// seven it knows (RFC 2863 names, lowercased).
func operstate(b []byte) string {
	switch string(b) {
	case "up":
		return "up"
	case "down":
		return "down"
	case "lowerlayerdown":
		return "lowerlayerdown"
	case "unknown":
		return "unknown"
	case "dormant":
		return "dormant"
	case "notpresent":
		return "notpresent"
	case "testing":
		return "testing"
	}
	return string(b)
}

// portFacts classifies one netdev by the rules of §2.1 (rule 1, the
// directory and the name, is the caller's) and reads what does not change
// while its ifindex stays the same.
func (f FS) portFacts(name, ifindex string) *portFacts {
	pf := &portFacts{name: name, ifindex: ifindex}
	dir := sysClassNet + "/" + name
	// 2. ARPHRD_ETHER.
	if t, err := f.ReadTrim(dir + "/type"); err != nil || t != "1" {
		return pf
	}
	ents, err := f.ReadDir(dir)
	if err != nil {
		return pf
	}
	var wireless, stacked, conduit, device, lower bool
	for _, e := range ents {
		switch n := e.Name(); {
		case n == "phy80211" || n == "wireless":
			wireless = true
		case n == "bridge" || n == "bonding" || n == "brif":
			stacked = true
		case n == "dsa":
			conduit = true
		case n == "device":
			device = true
		case strings.HasPrefix(n, "lower_"):
			lower = true
		}
	}
	uevent, _ := f.Read(dir + "/uevent")
	devtype := ueventValue(uevent, "DEVTYPE")
	// foreignPeer is rule 8's test: iflink names another netdev (a veth's
	// peer, a macvlan's or VLAN's parent) and no lower_* link says it is a
	// local one. iflink is an index in the peer's namespace and may collide
	// with a local ifindex, so it is never looked up here.
	foreignPeer := func() bool {
		iflink, err := f.ReadTrim(dir + "/iflink")
		return err == nil && iflink != "" && iflink != "0" && iflink != ifindex && !lower
	}
	switch {
	case wireless || devtype == "wlan": // 3. not wireless
		return pf
	case devtype == "dsa" || devtype == "wwan": // 4. a DSA user port, or a cellular modem in 802.3 mode (A2)
		pf.kind, pf.wired = hardwarePort, devtype == "dsa"
	case stacked || devtype == "bridge" || devtype == "bond" || devtype == "team": // 5.
		return pf
	case devtype == "vlan": // 5. A VLAN the host handed in (parent elsewhere) is like a veth (A2)
		if !foreignPeer() {
			return pf
		}
		pf.kind = virtualNIC
	case conduit: // 6. the DSA CPU conduit
		return pf
	case device: // 7. backed by a device
		pf.kind, pf.wired = hardwarePort, true
	case foreignPeer(): // 8. a veth or macvlan whose other end is in another namespace
		pf.kind = virtualNIC
	default: // 9. ifb, dummy, tun/tap, tunnels, stacked devices
		return pf
	}

	pf.label = name
	for _, p := range [...]string{dir + "/of_node/label", dir + "/device/of_node/label"} {
		if b, err := f.Read(p); err == nil {
			if l := dtString(b); l != "" {
				pf.label = l
				break
			}
		}
	}
	switch {
	case devtype == "wwan":
		pf.medium = "wireless"
	case f.exists(dir+"/of_node/sfp") || f.exists(dir+"/device/of_node/sfp"):
		pf.medium = "sfp"
	case pf.kind == virtualNIC || virtualDriver(f.driver(dir)):
		pf.medium = "virtual"
	default:
		pf.medium = "copper"
	}
	pf.mac, _ = f.ReadTrim(dir + "/address")
	return pf
}

// virtualDriver reports whether a netdev's driver is a hypervisor's
// paravirtual NIC. Xen's netfront registers as "vif" on the xen bus.
func virtualDriver(name string) bool {
	switch name {
	case "virtio_net", "vmxnet3", "hv_netvsc", "xen-netfront", "xen_netfront", "vif":
		return true
	}
	return false
}

// driver is the basename of a netdev's device/driver link, "" without one.
func (f FS) driver(dir string) string {
	target, err := os.Readlink(f.path(dir + "/device/driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func (f FS) exists(p string) bool {
	_, err := os.Lstat(f.path(p))
	return err == nil
}

// readAttr reads a small attribute file into buf with one open, one read and
// one close, and returns it without surrounding white space; nil when it
// cannot be read or is empty. os.ReadFile would also stat the file and
// allocate 4 KB, since sysfs reports every attribute as that long; this runs
// for every port on every push.
func (f FS) readAttr(buf []byte, p string) []byte {
	path := f.path(p)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	for err == syscall.EINTR {
		fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil
	}
	n, err := syscall.Read(fd, buf)
	for err == syscall.EINTR {
		n, err = syscall.Read(fd, buf)
	}
	syscall.Close(fd)
	if err != nil || n <= 0 {
		return nil
	}
	if b := bytes.TrimSpace(buf[:n]); len(b) > 0 {
		return b
	}
	return nil
}

// ueventValue finds KEY=value in a uevent file.
func ueventValue(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// dtString is the first string of a devicetree string property (NUL
// terminated, possibly a NUL-separated list).
func dtString(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\x00")
	return strings.TrimSpace(s)
}

// BoardPorts reads the port roles from /etc/board.json (OpenWrt): network.lan
// and network.wan name their ports as "device" (one name) and/or "ports" (a
// list), in the order of the case. A missing or malformed file, or a
// section of the wrong shape, yields no names for that role.
func (f FS) BoardPorts() BoardPorts {
	data, err := f.Read("/etc/board.json")
	if err != nil {
		return BoardPorts{}
	}
	var doc struct {
		Network map[string]json.RawMessage `json:"network"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return BoardPorts{}
	}
	return BoardPorts{LAN: boardNames(doc.Network["lan"]), WAN: boardNames(doc.Network["wan"])}
}

func boardNames(raw json.RawMessage) []string {
	var sec map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &sec) != nil {
		return nil
	}
	var names []string
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var device string
	if json.Unmarshal(sec["device"], &device) == nil {
		add(device)
	}
	var ports []json.RawMessage
	if json.Unmarshal(sec["ports"], &ports) == nil {
		for _, p := range ports {
			var n string
			if json.Unmarshal(p, &n) == nil {
				add(n)
			}
		}
	}
	return names
}

// compareNatural orders names with their digit runs compared as numbers
// ("lan2" < "lan10"), then byte-wise so that equal numbers ("lan01", "lan1")
// still get a fixed order.
func compareNatural(a, b string) int {
	x, y := a, b
	for x != "" && y != "" {
		if isDigit(x[0]) && isDigit(y[0]) {
			i, j := digitRun(x), digitRun(y)
			nx, ny := strings.TrimLeft(x[:i], "0"), strings.TrimLeft(y[:j], "0")
			if len(nx) != len(ny) {
				return len(nx) - len(ny)
			}
			if c := strings.Compare(nx, ny); c != 0 {
				return c
			}
			x, y = x[i:], y[j:]
			continue
		}
		if x[0] != y[0] {
			return int(x[0]) - int(y[0])
		}
		x, y = x[1:], y[1:]
	}
	if len(x) != len(y) {
		return len(x) - len(y)
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func digitRun(s string) int {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}
