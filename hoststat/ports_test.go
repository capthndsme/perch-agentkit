package hoststat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// loadTree materializes testdata/ports/<shape>.txt under a fresh temporary
// root and returns the root. The file describes a tree:
//
//	# comment            (before the first header)
//	-- path --           a file: the lines up to the next header, verbatim
//	                     (trailing blank lines dropped)
//	-- path/ --          a directory block, one entry per line, relative to it:
//	  name=value         a file holding value and a newline; \n, \t, \0, \\
//	                     and \xHH are unescaped
//	  name/              an empty directory
//	  name -> target     a symbolic link
//	  name               an empty file
//	                     (blank and # lines are skipped)
//
// Links and empty directories are made here rather than committed: module
// zips leave symbolic links out, git leaves empty directories out, and sysfs
// is full of both.
func loadTree(t testing.TB, shape string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "ports", shape+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var header string
	var body []string
	flush := func() {
		switch {
		case header == "":
		case strings.HasSuffix(header, "/"):
			dir := filepath.Join(root, header)
			mkdirAll(t, dir)
			for _, line := range body {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					treeEntry(t, dir, line)
				}
			}
		default:
			for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
				body = body[:len(body)-1]
			}
			content := strings.Join(body, "\n")
			if content != "" {
				content += "\n"
			}
			writeFile(t, filepath.Join(root, header), content)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) > 6 && strings.HasPrefix(line, "-- ") && strings.HasSuffix(line, " --") {
			flush()
			header, body = line[3:len(line)-3], nil
			continue
		}
		body = append(body, line)
	}
	flush()
	return root
}

func treeEntry(t testing.TB, dir, line string) {
	t.Helper()
	if name, target, ok := strings.Cut(line, " -> "); ok {
		p := filepath.Join(dir, name)
		mkdirAll(t, filepath.Dir(p))
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return
	}
	if strings.HasSuffix(line, "/") {
		mkdirAll(t, filepath.Join(dir, line))
		return
	}
	name, value, ok := strings.Cut(line, "=")
	content := ""
	if ok {
		content = unescape(value) + "\n"
	}
	writeFile(t, filepath.Join(dir, name), content)
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteString(`\x`)
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func mkdirAll(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t testing.TB, p, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// portsJSON is what a daemon puts on the wire.
func portsJSON(t testing.TB, ports []Port) string {
	t.Helper()
	b, err := json.Marshal(ports)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkPorts compares against the expected array, written one port per line.
func checkPorts(t testing.TB, got []Port, want string) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(want)); err != nil {
		t.Fatalf("bad expectation: %v", err)
	}
	if g := portsJSON(t, got); g != buf.String() {
		t.Fatalf("ports\n got: %s\nwant: %s", strings.ReplaceAll(g, "},{", "},\n      {"), strings.ReplaceAll(buf.String(), "},{", "},\n      {"))
	}
}

func names(ports []Port) string {
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = p.Name
	}
	return strings.Join(out, ",")
}

func TestPortsShapes(t *testing.T) {
	for _, tc := range []struct {
		shape string
		opts  PortOptions
		want  string
	}{
		{
			// The conduit is no port; eth1 is WAN by board.json although it
			// is not bridged; lan1 without cable: speed -1 left out.
			shape: "dsa-conduit",
			want: `[
			{"name":"eth1","label":"eth1","role":"wan","medium":"copper","mac":"02:00:00:00:01:01","adminUp":true,"carrier":false,"operstate":"down","carrierChanges":1},
			{"name":"lan1","label":"lan1","role":"lan","medium":"copper","mac":"02:00:00:00:01:04","adminUp":true,"carrier":false,"operstate":"lowerlayerdown","carrierChanges":2},
			{"name":"lan2","label":"lan2","role":"lan","medium":"copper","mac":"02:00:00:00:01:04","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":3},
			{"name":"lan3","label":"lan3","role":"lan","medium":"copper","mac":"02:00:00:00:01:04","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":5}]`,
		},
		{
			// wan is a bridge member and stays role wan; carrierChanges 0 is
			// reported, not left out.
			shape: "gmac-wan",
			want: `[
			{"name":"wan","label":"wan","role":"wan","medium":"copper","mac":"02:00:00:00:02:0d","adminUp":true,"carrier":false,"operstate":"down","carrierChanges":2},
			{"name":"lan1","label":"lan1","role":"lan","medium":"copper","mac":"02:00:00:00:02:0c","adminUp":true,"carrier":true,"operstate":"up","speedMbps":100,"duplex":"full","carrierChanges":7},
			{"name":"lan2","label":"lan2","role":"lan","medium":"copper","mac":"02:00:00:00:02:0c","adminUp":true,"carrier":false,"operstate":"lowerlayerdown","carrierChanges":0},
			{"name":"lan3","label":"lan3","role":"lan","medium":"copper","mac":"02:00:00:00:02:0c","adminUp":true,"carrier":false,"operstate":"lowerlayerdown","carrierChanges":0},
			{"name":"lan4","label":"lan4","role":"lan","medium":"copper","mac":"02:00:00:00:02:0c","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":3}]`,
		},
		{
			// lan1 is administratively down: no carrier, speed or duplex.
			shape: "independent-netdevs",
			want: `[
			{"name":"wan","label":"wan","role":"wan","medium":"copper","mac":"02:00:00:00:03:45","adminUp":true,"carrier":true,"operstate":"up","speedMbps":2500,"duplex":"full","carrierChanges":11},
			{"name":"lan1","label":"lan1","role":"lan","medium":"copper","mac":"02:00:00:00:03:46","adminUp":false,"operstate":"down","carrierChanges":0},
			{"name":"lan2","label":"lan2","role":"lan","medium":"copper","mac":"02:00:00:00:03:46","adminUp":true,"carrier":false,"operstate":"down","carrierChanges":0},
			{"name":"lan3","label":"lan3","role":"lan","medium":"copper","mac":"02:00:00:00:03:46","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":9},
			{"name":"lan4","label":"lan4","role":"lan","medium":"copper","mac":"02:00:00:00:03:46","adminUp":true,"carrier":false,"operstate":"down","carrierChanges":0}]`,
		},
		{
			// No hardware port: the veths are the ports. The WAN list comes
			// from the caller; board.json (the host's) naming the eth0 veth
			// LAN is ignored; the rest is in natural order. The ifb and the
			// macvlan are not ports, the VLAN handed in by the host is.
			shape: "container-veths",
			opts:  PortOptions{WAN: []string{"wan0", "wan2"}},
			want: `[
			{"name":"wan0","label":"wan0","role":"wan","medium":"virtual","mac":"02:00:00:00:04:35","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"wan2","label":"wan2","role":"wan","medium":"virtual","mac":"02:00:00:00:04:37","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"eth0","label":"eth0","medium":"virtual","mac":"02:00:00:00:04:21","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"guest","label":"guest","medium":"virtual","mac":"02:00:00:00:04:25","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"iot","label":"iot","medium":"virtual","mac":"02:00:00:00:04:27","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"lan0","label":"lan0","medium":"virtual","mac":"02:00:00:00:04:29","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"modem1","label":"modem1","medium":"virtual","mac":"02:00:00:00:04:23","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"modem2","label":"modem2","medium":"virtual","mac":"02:00:00:00:04:33","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"server","label":"server","medium":"virtual","mac":"02:00:00:00:04:31","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":2},
			{"name":"vlan10","label":"vlan10","medium":"virtual","mac":"02:00:00:00:04:41","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":1}]`,
		},
		{
			// Symlinked sysfs; virtio NICs are hardware-backed but "virtual"
			// by their driver; the docker veth is not reported (VirtualAuto
			// with hardware ports). No board.json: roles only from WAN.
			shape: "vm-virtio",
			opts:  PortOptions{WAN: []string{"ens3"}},
			want: `[
			{"name":"ens3","label":"ens3","role":"wan","medium":"virtual","mac":"02:00:00:00:05:02","adminUp":true,"carrier":true,"operstate":"up","carrierChanges":1},
			{"name":"ens4","label":"ens4","medium":"virtual","mac":"02:00:00:00:05:03","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":1}]`,
		},
		{
			// WAN in board order (sfp1, wan), LAN in board order (lan3,
			// lan1, lan2), then eth2, eth3, lan9, lan10, wwan0 in natural
			// order. Own labels win over the switch's; eth3's label and cage
			// are on its device's of_node; the LTE modem is "wireless".
			shape: "mixed-noise",
			want: `[
			{"name":"sfp1","label":"SFP","role":"wan","medium":"sfp","mac":"02:00:00:00:06:22","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10000,"duplex":"full","carrierChanges":3},
			{"name":"wan","label":"wan","role":"wan","medium":"copper","mac":"02:00:00:00:06:03","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":6},
			{"name":"lan3","label":"lan3","role":"lan","medium":"copper","mac":"02:00:00:00:06:02","adminUp":true,"carrier":true,"operstate":"up","speedMbps":10,"duplex":"half","carrierChanges":1},
			{"name":"lan1","label":"lan1","role":"lan","medium":"copper","mac":"02:00:00:00:06:02","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":4},
			{"name":"lan2","label":"lan2","role":"lan","medium":"copper","mac":"02:00:00:00:06:02","adminUp":true,"carrier":false,"operstate":"lowerlayerdown","carrierChanges":0},
			{"name":"eth2","label":"eth2","medium":"copper","mac":"02:00:00:00:06:20","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":1},
			{"name":"eth3","label":"sfp2","medium":"sfp","mac":"02:00:00:00:06:21","adminUp":true,"carrier":false,"operstate":"down","carrierChanges":0},
			{"name":"lan9","label":"lan9","medium":"copper","mac":"02:00:00:00:06:23","adminUp":true,"carrier":false,"operstate":"down","carrierChanges":0},
			{"name":"lan10","label":"lan10","medium":"copper","mac":"02:00:00:00:06:30","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":2},
			{"name":"wwan0","label":"wwan0","medium":"wireless","mac":"02:00:00:00:06:47","adminUp":true,"carrier":true,"operstate":"unknown","carrierChanges":1}]`,
		},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			root := loadTree(t, tc.shape)
			checkPorts(t, FS{Root: root}.Ports(tc.opts), tc.want)
			// The reader answers the same, twice (the second read from the
			// cache).
			r := &PortReader{FS: FS{Root: root}, Options: tc.opts}
			checkPorts(t, r.Read(), tc.want)
			checkPorts(t, r.Read(), tc.want)
		})
	}
}

// Each rule of §2.1 on its own: every case is one netdev that a single rule
// decides (with VirtualAlways, so a virtual NIC would show).
func TestPortRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		medium  string // "" = not a port
	}{
		{"device", []string{"type=1", "ifindex=2", "iflink=2", "device/"}, "copper"},
		{"7 before 8: device and a foreign iflink", []string{"type=1", "ifindex=2", "iflink=9", "device/"}, "copper"},
		{"2: not ARPHRD_ETHER", []string{"type=280", "ifindex=2", "iflink=2", "device/"}, ""},
		{"2: no type", []string{"ifindex=2", "iflink=2", "device/"}, ""},
		{"3: phy80211", []string{"type=1", "ifindex=2", "iflink=2", "device/", "phy80211/"}, ""},
		{"3: wireless", []string{"type=1", "ifindex=2", "iflink=2", "device/", "wireless/"}, ""},
		{"3: DEVTYPE=wlan", []string{"type=1", "ifindex=2", "iflink=2", "device/", `uevent=DEVTYPE=wlan\nINTERFACE=x`}, ""},
		{"3 before 4: a wireless DSA port", []string{"type=1", "ifindex=2", "iflink=5", "phy80211/", `uevent=DEVTYPE=dsa`}, ""},
		{"4: DSA without a device link", []string{"type=1", "ifindex=2", "iflink=5", "lower_eth0", `uevent=DEVTYPE=dsa\nINTERFACE=x`}, "copper"},
		{"4 before 6: a DSA port with dsa/", []string{"type=1", "ifindex=2", "iflink=5", "dsa/tagging=x", `uevent=DEVTYPE=dsa`}, "copper"},
		{"4: an LTE modem", []string{"type=1", "ifindex=2", "iflink=2", "device/", `uevent=DEVTYPE=wwan\nINTERFACE=x`}, "wireless"},
		{"4: an LTE modem without a device link", []string{"type=1", "ifindex=2", "iflink=2", `uevent=DEVTYPE=wwan`}, "wireless"},
		{"2 before 4: an LTE modem in raw-IP mode", []string{"type=65534", "ifindex=2", "iflink=2", "device/", `uevent=DEVTYPE=wwan`}, ""},
		{"5: DEVTYPE=bridge", []string{"type=1", "ifindex=2", "iflink=2", "device/", `uevent=DEVTYPE=bridge`}, ""},
		{"5: a VLAN on a local device", []string{"type=1", "ifindex=2", "iflink=9", "lower_eth0", `uevent=DEVTYPE=vlan`}, ""},
		{"5: a VLAN on a local device with a device link", []string{"type=1", "ifindex=2", "iflink=9", "lower_eth0", "device/", `uevent=DEVTYPE=vlan`}, ""},
		{"5: a VLAN handed in by the host", []string{"type=1", "ifindex=2", "iflink=9", `uevent=DEVTYPE=vlan`}, "virtual"},
		{"5: a VLAN with iflink == ifindex", []string{"type=1", "ifindex=2", "iflink=2", `uevent=DEVTYPE=vlan`}, ""},
		{"5: DEVTYPE=bond", []string{"type=1", "ifindex=2", "iflink=2", "device/", `uevent=DEVTYPE=bond`}, ""},
		{"5: DEVTYPE=team", []string{"type=1", "ifindex=2", "iflink=9", `uevent=DEVTYPE=team`}, ""},
		{"5: bridge/", []string{"type=1", "ifindex=2", "iflink=2", "device/", "bridge/"}, ""},
		{"5: bonding/", []string{"type=1", "ifindex=2", "iflink=9", "bonding/"}, ""},
		{"5: brif/", []string{"type=1", "ifindex=2", "iflink=9", "brif/"}, ""},
		{"5 not for members: brport/", []string{"type=1", "ifindex=2", "iflink=2", "device/", "brport/", "master"}, "copper"},
		{"5 not for members: bonding_slave/", []string{"type=1", "ifindex=2", "iflink=2", "device/", "bonding_slave/"}, "copper"},
		{"6: the DSA conduit", []string{"type=1", "ifindex=2", "iflink=2", "device/", "dsa/tagging=mtk"}, ""},
		{"8: a veth", []string{"type=1", "ifindex=2", "iflink=9"}, "virtual"},
		{"8: stacked on a local device", []string{"type=1", "ifindex=2", "iflink=9", "lower_eth0"}, ""},
		{"9: iflink == ifindex", []string{"type=1", "ifindex=2", "iflink=2"}, ""},
		{"9: iflink 0", []string{"type=1", "ifindex=2", "iflink=0"}, ""},
		{"9: no iflink", []string{"type=1", "ifindex=2"}, ""},
		{"sfp in of_node", []string{"type=1", "ifindex=2", "iflink=2", "device/", "of_node/sfp"}, "sfp"},
		{"sfp in device/of_node", []string{"type=1", "ifindex=2", "iflink=2", "device/of_node/sfp"}, "sfp"},
		{"virtio_net", []string{"type=1", "ifindex=2", "iflink=2", "device/driver -> ../../bus/virtio/drivers/virtio_net"}, "virtual"},
		{"vmxnet3", []string{"type=1", "ifindex=2", "iflink=2", "device/driver -> ../../bus/pci/drivers/vmxnet3"}, "virtual"},
		{"hv_netvsc", []string{"type=1", "ifindex=2", "iflink=2", "device/driver -> ../../bus/vmbus/drivers/hv_netvsc"}, "virtual"},
		{"xen-netfront", []string{"type=1", "ifindex=2", "iflink=2", "device/driver -> ../../bus/xen/drivers/xen-netfront"}, "virtual"},
		{"xen vif", []string{"type=1", "ifindex=2", "iflink=2", "device/driver -> ../../bus/xen/drivers/vif"}, "virtual"},
		{"e1000e", []string{"type=1", "ifindex=2", "iflink=2", "device/driver -> ../../bus/pci/drivers/e1000e"}, "copper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "sys/class/net/x")
			mkdirAll(t, dir)
			for _, e := range tc.entries {
				treeEntry(t, dir, e)
			}
			got := FS{Root: root}.Ports(PortOptions{Virtual: VirtualAlways})
			switch {
			case tc.medium == "" && len(got) != 0:
				t.Fatalf("reported %+v", got)
			case tc.medium != "" && (len(got) != 1 || got[0].Medium != tc.medium):
				t.Fatalf("got %+v, want one %s port", got, tc.medium)
			}
		})
	}
	// Rule 1: a file is not a netdev, whatever it holds.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sys/class/net/bonding_masters"), "bond0\n")
	if got := (FS{Root: root}).Ports(PortOptions{Virtual: VirtualAlways}); got == nil || len(got) != 0 {
		t.Fatalf("bonding_masters: %+v", got)
	}
}

func TestPortsRolesAndOrder(t *testing.T) {
	root := loadTree(t, "mixed-noise")
	fs := FS{Root: root}
	// A caller's WAN interface joins the WAN group after the board's.
	got := fs.Ports(PortOptions{WAN: []string{"eth2", "not-a-port"}})
	if n := names(got); n != "sfp1,wan,eth2,lan3,lan1,lan2,eth3,lan9,lan10,wwan0" {
		t.Fatalf("order %s", n)
	}
	if got[2].Role != "wan" || got[6].Role != "" {
		t.Fatalf("roles %+v", got)
	}
	// Board WAN wins over board LAN.
	writeFile(t, filepath.Join(root, "etc/board.json"), `{"network":{"lan":{"device":"lan1","ports":["lan2","lan1"]},"wan":{"device":"lan1"}}}`)
	got = fs.Ports(PortOptions{})
	if n := names(got); n != "lan1,lan2,eth2,eth3,lan3,lan9,lan10,sfp1,wan,wwan0" {
		t.Fatalf("order after board change %s", n)
	}
	if got[0].Role != "wan" || got[1].Role != "lan" || got[4].Role != "" {
		t.Fatalf("roles after board change %+v", got[:5])
	}
}

// board.json describes the hardware: its roles never apply to a virtual NIC
// (in a container it is the host's file). The caller's WAN list applies to
// every port.
func TestBoardRolesOnlyForHardwarePorts(t *testing.T) {
	root := t.TempDir()
	for name, entries := range map[string][]string{
		"e1": {"type=1", "ifindex=2", "iflink=2", "device/"},
		"v1": {"type=1", "ifindex=3", "iflink=30"},
		"v2": {"type=1", "ifindex=4", "iflink=40"},
	} {
		for _, e := range entries {
			treeEntry(t, filepath.Join(root, "sys/class/net", name), e)
		}
	}
	writeFile(t, filepath.Join(root, "etc/board.json"), `{"network":{"lan":{"ports":["v1","e1"]},"wan":{"device":"v2"}}}`)
	got := FS{Root: root}.Ports(PortOptions{Virtual: VirtualAlways, WAN: []string{"v1"}})
	if names(got) != "v1,e1,v2" || got[0].Role != "wan" || got[1].Role != "lan" || got[2].Role != "" {
		t.Fatalf("%+v", got)
	}
	// Without hardware, VirtualAuto reports the veths, still without board roles.
	if err := os.RemoveAll(filepath.Join(root, "sys/class/net/e1")); err != nil {
		t.Fatal(err)
	}
	got = FS{Root: root}.Ports(PortOptions{})
	if names(got) != "v1,v2" || got[0].Role != "" || got[1].Role != "" {
		t.Fatalf("auto: %+v", got)
	}
}

func TestPortsLabelFromDeviceOfNode(t *testing.T) {
	root := loadTree(t, "independent-netdevs")
	writeFile(t, filepath.Join(root, "sys/class/net/lan4/device/of_node/label"), "Port 4\x00")
	got := FS{Root: root}.Ports(PortOptions{})
	if got[4].Name != "lan4" || got[4].Label != "Port 4" {
		t.Fatalf("lan4 %+v", got[4])
	}
	// An own of_node label wins over the device's.
	writeFile(t, filepath.Join(root, "sys/class/net/lan4/of_node/label"), "LAN4\x00spare\x00")
	if got := (FS{Root: root}).Ports(PortOptions{}); got[4].Label != "LAN4" {
		t.Fatalf("lan4 %+v", got[4])
	}
}

func TestPortsVirtualPolicy(t *testing.T) {
	container := FS{Root: loadTree(t, "container-veths")}
	all := "eth0,guest,iot,lan0,modem1,modem2,server,vlan10,wan0,wan2"
	if got := names(container.Ports(PortOptions{})); got != all {
		t.Fatalf("auto without hardware: %s", got)
	}
	if got := names(container.Ports(PortOptions{Virtual: VirtualAlways})); got != all {
		t.Fatalf("always: %s", got)
	}
	if got := names(container.Ports(PortOptions{Virtual: "bogus"})); got != all {
		t.Fatalf("an unknown policy is auto: %s", got)
	}
	never := container.Ports(PortOptions{Virtual: VirtualNever})
	if never == nil || len(never) != 0 || portsJSON(t, never) != "[]" {
		t.Fatalf("never: %#v", never)
	}

	// A container gateway with an LTE modem passed through: the modem is no
	// wired port, so the veths stay and the modem is reported beside them.
	// A wired NIC passed through hides the veths again; virtio counts as
	// wired (below).
	root := loadTree(t, "container-veths")
	for _, e := range []string{"type=1", "ifindex=50", "iflink=50", "flags=0x1003", "operstate=unknown", "device/", `uevent=DEVTYPE=wwan\nINTERFACE=wwan0`} {
		treeEntry(t, filepath.Join(root, "sys/class/net/wwan0"), e)
	}
	lte := FS{Root: root}
	got := lte.Ports(PortOptions{WAN: []string{"wwan0"}})
	if names(got) != "wwan0,eth0,guest,iot,lan0,modem1,modem2,server,vlan10,wan0,wan2" || got[0].Medium != "wireless" || got[0].Role != "wan" {
		t.Fatalf("container with a modem: %s %+v", names(got), got[0])
	}
	if got := names(lte.Ports(PortOptions{Virtual: VirtualNever})); got != "wwan0" {
		t.Fatalf("container with a modem, never: %s", got)
	}
	for _, e := range []string{"type=1", "ifindex=51", "iflink=51", "flags=0x1003", "operstate=up", "device/"} {
		treeEntry(t, filepath.Join(root, "sys/class/net/eth9"), e)
	}
	if got := names(lte.Ports(PortOptions{WAN: []string{"wwan0"}})); got != "wwan0,eth9" {
		t.Fatalf("container with a modem and a wired NIC: %s", got)
	}

	// DSA user ports are wired ports too: a switch-only router hides a veth.
	dsa := t.TempDir()
	for name, entries := range map[string][]string{
		"lan1": {"type=1", "ifindex=3", "iflink=2", "lower_eth0", "device/", `uevent=DEVTYPE=dsa`},
		"v1":   {"type=1", "ifindex=4", "iflink=40"},
	} {
		for _, e := range entries {
			treeEntry(t, filepath.Join(dsa, "sys/class/net", name), e)
		}
	}
	if got := names(FS{Root: dsa}.Ports(PortOptions{})); got != "lan1" {
		t.Fatalf("DSA ports and a veth, auto: %s", got)
	}

	vm := FS{Root: loadTree(t, "vm-virtio")}
	if got := names(vm.Ports(PortOptions{})); got != "ens3,ens4" {
		t.Fatalf("vm auto: %s", got)
	}
	if got := names(vm.Ports(PortOptions{Virtual: VirtualNever})); got != "ens3,ens4" {
		t.Fatalf("vm never: %s", got)
	}
	got = vm.Ports(PortOptions{Virtual: VirtualAlways})
	if names(got) != "ens3,ens4,veth1a2b3c4" || got[2].Medium != "virtual" || got[2].Role != "" {
		t.Fatalf("vm always: %+v", got)
	}

	// With every kind of noise around, VirtualAlways adds exactly the veth:
	// the macvlan and the VLAN have lower_* links, ifb/dummy/tap have
	// iflink == ifindex, the tunnel has iflink 0.
	mixed := FS{Root: loadTree(t, "mixed-noise")}
	if got := names(mixed.Ports(PortOptions{Virtual: VirtualAlways})); got != "sfp1,wan,lan3,lan1,lan2,eth2,eth3,lan9,lan10,veth0,wwan0" {
		t.Fatalf("mixed always: %s", got)
	}
}

func TestPortsMax(t *testing.T) {
	container := FS{Root: loadTree(t, "container-veths")}
	got := container.Ports(PortOptions{WAN: []string{"wan0", "wan2"}, Max: 3})
	if names(got) != "wan0,wan2,eth0" {
		t.Fatalf("max 3: %s", names(got))
	}

	// 70 veths: 64 by default, in natural order.
	root := t.TempDir()
	for i := 1; i <= 70; i++ {
		dir := filepath.Join(root, "sys/class/net", fmt.Sprintf("v%d", i))
		for name, value := range map[string]string{"type": "1", "ifindex": strconv.Itoa(100 + i), "iflink": strconv.Itoa(200 + i), "flags": "0x1003", "operstate": "up"} {
			writeFile(t, filepath.Join(dir, name), value+"\n")
		}
	}
	got = FS{Root: root}.Ports(PortOptions{})
	if len(got) != 64 || got[0].Name != "v1" || got[8].Name != "v9" || got[9].Name != "v10" || got[63].Name != "v64" {
		t.Fatalf("default cap: %d ports, %s", len(got), names(got))
	}
	if got := (FS{Root: root}).Ports(PortOptions{Max: 100}); len(got) != 70 {
		t.Fatalf("max 100: %d", len(got))
	}
}

func TestPortsWithoutSysfs(t *testing.T) {
	root := t.TempDir()
	if got := (FS{Root: root}).Ports(PortOptions{}); got != nil {
		t.Fatalf("no /sys/class/net: %#v, want nil", got)
	}
	writeFile(t, filepath.Join(root, "sys/class/net/lo/type"), "772\n")
	writeFile(t, filepath.Join(root, "sys/class/net/lo/ifindex"), "1\n")
	writeFile(t, filepath.Join(root, "sys/class/net/bonding_masters"), "\n")
	got := FS{Root: root}.Ports(PortOptions{})
	if got == nil || len(got) != 0 || portsJSON(t, got) != "[]" {
		t.Fatalf("only lo: %#v, want an empty slice", got)
	}
}

func TestPortReaderCache(t *testing.T) {
	root := loadTree(t, "gmac-wan")
	net := filepath.Join(root, "sys/class/net")
	clock := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	r := &PortReader{FS: FS{Root: root}, now: func() time.Time { return clock }}
	find := func(ports []Port, name string) Port {
		t.Helper()
		for _, p := range ports {
			if p.Name == name {
				return p
			}
		}
		t.Fatalf("%s missing from %s", name, names(ports))
		return Port{}
	}
	r.Read()
	if len(r.facts) != 11 { // every netdev directory but lo
		t.Fatalf("%d cached netdevs", len(r.facts))
	}

	// Link state is never cached.
	for f, v := range map[string]string{"carrier": "1", "operstate": "up", "speed": "1000", "duplex": "full", "carrier_changes": "1"} {
		writeFile(t, filepath.Join(net, "lan2", f), v+"\n")
	}
	writeFile(t, filepath.Join(net, "lan4/flags"), "0x1002\n")
	os.Remove(filepath.Join(net, "lan4/carrier"))
	got := r.Read()
	if lan2 := find(got, "lan2"); !*lan2.Carrier || lan2.Operstate != "up" || *lan2.SpeedMbps != 1000 || lan2.Duplex != "full" || *lan2.CarrierChanges != 1 {
		t.Fatalf("lan2 after the cable went in: %+v", lan2)
	}
	if lan4 := find(got, "lan4"); *lan4.AdminUp || lan4.Carrier != nil {
		t.Fatalf("lan4 after ifdown: %+v", lan4)
	}

	// Facts are cached per name+ifindex: a new label shows once the netdev
	// is re-created (new ifindex) or the TTL is over.
	writeFile(t, filepath.Join(net, "lan4/of_node/label"), "port4\x00")
	writeFile(t, filepath.Join(net, "lan3/of_node/label"), "port3\x00")
	got = r.Read()
	if find(got, "lan4").Label != "lan4" || find(got, "lan3").Label != "lan3" {
		t.Fatalf("labels re-read inside the TTL: %+v", got)
	}
	writeFile(t, filepath.Join(net, "lan4/ifindex"), "17\n")
	got = r.Read()
	if find(got, "lan4").Label != "port4" || find(got, "lan3").Label != "lan3" {
		t.Fatalf("labels after lan4 was re-created: %+v", got)
	}
	clock = clock.Add(5 * time.Minute)
	if got := r.Read(); find(got, "lan3").Label != "port3" {
		t.Fatalf("lan3 after the TTL: %+v", find(got, "lan3"))
	}

	// board.json is cached for the TTL too; the caller's WAN list is not.
	writeFile(t, filepath.Join(root, "etc/board.json"), `{"network":{"lan":{"ports":["wan","lan1"]}}}`)
	if got := r.Read(); got[0].Name != "wan" || got[0].Role != "wan" {
		t.Fatalf("board.json re-read inside the TTL: %+v", got[0])
	}
	r.Options.WAN = []string{"lan3"}
	if got := r.Read(); names(got) != "wan,lan3,lan1,lan2,lan4" || got[1].Role != "wan" {
		t.Fatalf("with WAN lan3: %s %+v", names(got), got[1])
	}
	clock = clock.Add(5 * time.Minute)
	got = r.Read()
	if names(got) != "lan3,wan,lan1,lan2,lan4" || got[1].Role != "lan" || got[4].Role != "" {
		t.Fatalf("after the board TTL: %s %+v", names(got), got)
	}

	// A netdev that goes away leaves the result and the cache at once; a
	// clock that steps back re-reads the facts instead of trusting them.
	if err := os.RemoveAll(filepath.Join(net, "lan3")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(net, "lan1/of_node/label"), "uplink\x00")
	clock = clock.Add(-time.Hour)
	got = r.Read()
	if names(got) != "wan,lan1,lan2,lan4" || len(r.facts) != 10 || find(got, "lan1").Label != "uplink" {
		t.Fatalf("after removing lan3: %s, %d cached, %+v", names(got), len(r.facts), got)
	}
}

func TestPortReaderConcurrentReads(t *testing.T) {
	r := &PortReader{FS: FS{Root: loadTree(t, "mixed-noise")}, TTL: time.Nanosecond}
	want := names(r.Read())
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if got := names(r.Read()); got != want {
					errs <- got
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Errorf("concurrent read %s, want %s", got, want)
	}
}

func TestBoardPorts(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       BoardPorts
	}{
		{"missing", "", BoardPorts{}},
		{"malformed", `{"network":`, BoardPorts{}},
		{"no network", `{"model":{"id":"x"}}`, BoardPorts{}},
		{"ports and device", `{"network":{"lan":{"ports":["lan1","lan2"],"protocol":"static"},"wan":{"device":"wan","protocol":"dhcp"}}}`,
			BoardPorts{LAN: []string{"lan1", "lan2"}, WAN: []string{"wan"}}},
		{"device first, no repeats", `{"network":{"lan":{"device":"eth0","ports":["eth0","eth1"]},"wan":{"ports":["wan","sfp"]}}}`,
			BoardPorts{LAN: []string{"eth0", "eth1"}, WAN: []string{"wan", "sfp"}}},
		{"wrong types are skipped", `{"network":{"lan":{"device":7,"ports":["lan1",8,null,"","lan2"]},"wan":"eth1","guest":[1]}}`,
			BoardPorts{LAN: []string{"lan1", "lan2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.json != "" {
				writeFile(t, filepath.Join(root, "etc/board.json"), tc.json)
			}
			if got := (FS{Root: root}).BoardPorts(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCompareNatural(t *testing.T) {
	want := []string{"a0", "a00", "eth", "eth0", "lan001", "lan01", "lan1", "lan1.100", "lan1a", "lan2", "lan10", "sfp1", "wan", "x9y2", "x9y10"}
	for i := range want {
		for j := range want {
			c := compareNatural(want[i], want[j])
			if (i < j && c >= 0) || (i > j && c <= 0) || (i == j && c != 0) {
				t.Errorf("compareNatural(%q, %q) = %d", want[i], want[j], c)
			}
		}
	}
}

// The machine running the tests: whatever it has, nothing may be malformed.
func TestLivePorts(t *testing.T) {
	if _, err := os.Stat(sysClassNet); err != nil {
		t.Skip("no /sys/class/net")
	}
	ports := FS{}.Ports(PortOptions{Virtual: VirtualAlways})
	if ports == nil {
		t.Fatal("nil from a readable /sys/class/net")
	}
	for _, p := range ports {
		if p.Name == "" || p.Name == "lo" || p.Label == "" || (p.Medium != "copper" && p.Medium != "sfp" && p.Medium != "virtual") {
			t.Errorf("malformed port %+v", p)
		}
		if p.SpeedMbps != nil && *p.SpeedMbps <= 0 || p.Duplex != "" && p.Duplex != "full" && p.Duplex != "half" {
			t.Errorf("bad link fields %+v", p)
		}
	}
	t.Logf("%d ports: %s", len(ports), names(ports))
}

// One read of a four-port access point in steady state (facts cached).
func BenchmarkPortReaderRead(b *testing.B) {
	r := &PortReader{FS: FS{Root: loadTree(b, "gmac-wan")}}
	if len(r.Read()) != 5 {
		b.Fatal("fixture")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Read()
	}
}
