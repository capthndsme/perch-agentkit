// Package hoststat reads the kernel's view of the host from /proc: load,
// memory, interface counters, connection tracking, SNMP counters and the
// default routes. Every reader goes through FS so tests (and the daemons'
// tests) can point it at a fixture tree instead of /.
package hoststat

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FS reads files under a root.
type FS struct{ Root string }

func (f FS) path(p string) string {
	if f.Root == "" || f.Root == "/" {
		return p
	}
	return filepath.Join(f.Root, p)
}

// Read returns a file's contents.
func (f FS) Read(p string) ([]byte, error) { return os.ReadFile(f.path(p)) }

// ReadTrim returns a file's contents without surrounding whitespace.
func (f FS) ReadTrim(p string) (string, error) {
	b, err := f.Read(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ReadDir lists a directory.
func (f FS) ReadDir(p string) ([]os.DirEntry, error) { return os.ReadDir(f.path(p)) }

// Load is /proc/loadavg's first three fields.
type Load struct{ Load1, Load5, Load15 float64 }

// Loadavg reads /proc/loadavg.
func (f FS) Loadavg() (Load, error) {
	s, err := f.ReadTrim("/proc/loadavg")
	if err != nil {
		return Load{}, err
	}
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return Load{}, errors.New("short /proc/loadavg")
	}
	var v [3]float64
	for i := range v {
		if v[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return Load{}, err
		}
	}
	return Load{v[0], v[1], v[2]}, nil
}

// MemEntry is one /proc/meminfo line: its raw key ("MemTotal",
// "Active(anon)") and value, in bytes when the file says kB.
type MemEntry struct {
	Key   string
	Value uint64
}

// Meminfo reads /proc/meminfo in file order. Lines that do not parse are
// skipped.
func (f FS) Meminfo() ([]MemEntry, error) {
	data, err := f.Read("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	var out []MemEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}
		out = append(out, MemEntry{Key: strings.TrimSpace(key), Value: v})
	}
	return out, nil
}

// MemValue finds one key in Meminfo's result.
func MemValue(entries []MemEntry, key string) (uint64, bool) {
	for _, e := range entries {
		if e.Key == key {
			return e.Value, true
		}
	}
	return 0, false
}

// NetDevFields names the sixteen counters of /proc/net/dev in file order.
var NetDevFields = [16]string{
	"receive_bytes", "receive_packets", "receive_errs", "receive_drop",
	"receive_fifo", "receive_frame", "receive_compressed", "receive_multicast",
	"transmit_bytes", "transmit_packets", "transmit_errs", "transmit_drop",
	"transmit_fifo", "transmit_colls", "transmit_carrier", "transmit_compressed",
}

// NetDev is one interface's /proc/net/dev line.
type NetDev struct {
	Name     string
	Counters [16]uint64
}

// RxBytes is the receive byte counter.
func (d NetDev) RxBytes() uint64 { return d.Counters[0] }

// TxBytes is the transmit byte counter.
func (d NetDev) TxBytes() uint64 { return d.Counters[8] }

// NetDev reads /proc/net/dev in file order. Lines with fewer than sixteen
// counters are skipped; a counter that does not parse reads as 0.
func (f FS) NetDev() ([]NetDev, error) {
	data, err := f.Read("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	var out []NetDev
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines have no colon
		}
		fields := strings.Fields(rest)
		if len(fields) < len(NetDevFields) {
			continue
		}
		d := NetDev{Name: strings.TrimSpace(name)}
		for i := range NetDevFields {
			if v, err := strconv.ParseUint(fields[i], 10, 64); err == nil {
				d.Counters[i] = v
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// Conntrack is the connection-tracking table fill. Either half may be
// missing (no nf_conntrack module, or a container that hides the limit).
type Conntrack struct {
	Entries, Limit       uint64
	HasEntries, HasLimit bool
}

// Conntrack reads nf_conntrack_count and nf_conntrack_max.
func (f FS) Conntrack() Conntrack {
	var c Conntrack
	if s, err := f.ReadTrim("/proc/sys/net/netfilter/nf_conntrack_count"); err == nil {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			c.Entries, c.HasEntries = v, true
		}
	}
	if s, err := f.ReadTrim("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			c.Limit, c.HasLimit = v, true
		}
	}
	return c
}

// Snmp reads /proc/net/snmp: per protocol ("Tcp", "Udp", …) a header line of
// names and a line of values, returned as protocol → name → value.
func (f FS) Snmp() (map[string]map[string]int64, error) {
	data, err := f.Read("/proc/net/snmp")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int64{}
	var header []string
	var headerProto string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		proto, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if header == nil || headerProto != proto {
			header, headerProto = fields, proto
			continue
		}
		values := map[string]int64{}
		for i, name := range header {
			if i >= len(fields) {
				break
			}
			if v, err := strconv.ParseInt(fields[i], 10, 64); err == nil {
				values[name] = v
			}
		}
		out[proto] = values
		header, headerProto = nil, ""
	}
	return out, nil
}

// Route flags from linux/route.h and linux/ipv6_route.h.
const (
	rtfUp     = 0x0001
	rtfReject = 0x0200
)

// DefaultRouteInterfaces lists the interfaces that hold a default route in
// the main table: IPv4 rows of /proc/net/route with destination and mask 0,
// IPv6 rows of /proc/net/ipv6_route for ::/0. Routes that are down or reject
// routes (and everything on lo) do not count. Sorted and unique. An error
// only when neither file can be read.
func (f FS) DefaultRouteInterfaces() ([]string, error) {
	seen := map[string]bool{}
	v4, err4 := f.Read("/proc/net/route")
	if err4 == nil {
		sc := bufio.NewScanner(bytes.NewReader(v4))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 8 || fields[0] == "Iface" {
				continue
			}
			flags, err := strconv.ParseUint(fields[3], 16, 32)
			if err != nil || fields[1] != "00000000" || fields[7] != "00000000" {
				continue
			}
			if flags&rtfUp != 0 && flags&rtfReject == 0 && fields[0] != "lo" {
				seen[fields[0]] = true
			}
		}
	}
	v6, err6 := f.Read("/proc/net/ipv6_route")
	if err6 == nil {
		sc := bufio.NewScanner(bytes.NewReader(v6))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[1] != "00" || strings.Trim(fields[0], "0") != "" {
				continue
			}
			flags, err := strconv.ParseUint(fields[8], 16, 32)
			if err != nil {
				continue
			}
			if flags&rtfUp != 0 && flags&rtfReject == 0 && fields[9] != "lo" {
				seen[fields[9]] = true
			}
		}
	}
	if err4 != nil && err6 != nil {
		return nil, err4
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
