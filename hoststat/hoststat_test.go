package hoststat

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixture(t *testing.T, files map[string]string) FS {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return FS{Root: root}
}

// A dual-WAN OpenWrt gateway: two IPv4 default routes, IPv6 unreachable
// defaults on lo, one real IPv6 default and one reject route.
const routeV4 = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wan0	00000000	017100CB	0003	0	0	1	00000000	0	0	0
wan2	00000000	0102A8C0	0003	0	0	2	00000000	0	0	0
lan0	086433C6	5C01A8C0	0007	0	0	0	FFFFFFFF	0	0	0
wan0	007100CB	00000000	0001	0	0	1	00F0FFFF	0	0	0
down0	00000000	01010101	0002	0	0	9	00000000	0	0	0
`

const routeV6 = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200 lo
00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 pppoe-wan
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000201 blackhole0
20010db8000000000000000000000000 20 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001 br-lan
`

func TestDefaultRouteInterfaces(t *testing.T) {
	fs := fixture(t, map[string]string{"proc/net/route": routeV4, "proc/net/ipv6_route": routeV6})
	got, err := fs.DefaultRouteInterfaces()
	if err != nil || !reflect.DeepEqual(got, []string{"pppoe-wan", "wan0", "wan2"}) {
		t.Fatalf("%v %v", got, err)
	}
	// IPv4 only (no IPv6 in the kernel) still works; neither file is an error.
	fs = fixture(t, map[string]string{"proc/net/route": routeV4})
	if got, err := fs.DefaultRouteInterfaces(); err != nil || !reflect.DeepEqual(got, []string{"wan0", "wan2"}) {
		t.Fatalf("v4 only: %v %v", got, err)
	}
	if _, err := fixture(t, nil).DefaultRouteInterfaces(); err == nil {
		t.Fatal("no route files and no error")
	}
}

func TestLoadMemNetdevConntrackSnmp(t *testing.T) {
	fs := fixture(t, map[string]string{
		"proc/loadavg": "1.44 1.10 1.49 1/2336 3075337\n",
		"proc/meminfo": "MemTotal:       15271332 kB\nMemAvailable:   15161130 kB\nActive(anon):     1024 kB\nHugePages_Total:       0\nbroken line\n",
		"proc/net/dev": "Inter-|   Receive                                                |  Transmit\n" +
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
			"    lo: 13399861   1000    0    0    0     0          0         0 13399861   1000    0    0    0     0       0          0\n" +
			"  wan0: 693974698743 5 0 0 0 0 0 0 1697321558462 6 0 0 0 0 0 0\n" +
			" short: 1 2 3\n",
		"proc/sys/net/netfilter/nf_conntrack_count": "2495\n",
		"proc/sys/net/netfilter/nf_conntrack_max":   "262144\n",
		"proc/net/snmp": "Ip: Forwarding DefaultTTL\nIp: 1 64\n" +
			"Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs\n" +
			"Tcp: 1 200 120000 -1 650 19565 280 13 2 1459927\n",
	})
	load, err := fs.Loadavg()
	if err != nil || load != (Load{1.44, 1.10, 1.49}) {
		t.Fatalf("load %+v %v", load, err)
	}
	mem, err := fs.Meminfo()
	if err != nil || len(mem) != 4 || mem[2] != (MemEntry{"Active(anon)", 1024 * 1024}) || mem[3] != (MemEntry{"HugePages_Total", 0}) {
		t.Fatalf("meminfo %+v %v", mem, err)
	}
	if v, ok := MemValue(mem, "MemAvailable"); !ok || v != 15161130*1024 {
		t.Fatalf("MemAvailable %d %v", v, ok)
	}
	devs, err := fs.NetDev()
	if err != nil || len(devs) != 2 || devs[1].Name != "wan0" || devs[1].RxBytes() != 693974698743 || devs[1].TxBytes() != 1697321558462 {
		t.Fatalf("netdev %+v %v", devs, err)
	}
	if ct := fs.Conntrack(); ct != (Conntrack{2495, 262144, true, true}) {
		t.Fatalf("conntrack %+v", ct)
	}
	snmp, err := fs.Snmp()
	if err != nil || snmp["Tcp"]["CurrEstab"] != 2 || snmp["Tcp"]["MaxConn"] != -1 || snmp["Ip"]["Forwarding"] != 1 {
		t.Fatalf("snmp %+v %v", snmp, err)
	}
}

func TestMissingFiles(t *testing.T) {
	fs := fixture(t, map[string]string{"proc/sys/net/netfilter/nf_conntrack_count": "7"})
	if ct := fs.Conntrack(); ct != (Conntrack{Entries: 7, HasEntries: true}) {
		t.Fatalf("conntrack without max %+v", ct)
	}
	if _, err := fs.Loadavg(); err == nil {
		t.Fatal("loadavg without a file")
	}
	if _, err := fs.Snmp(); err == nil {
		t.Fatal("snmp without a file")
	}
}

// The real kernel of whatever runs the tests: nothing should fail to parse.
func TestLiveProc(t *testing.T) {
	fs := FS{}
	if _, err := os.Stat("/proc/loadavg"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := fs.Loadavg(); err != nil {
		t.Error(err)
	}
	if mem, err := fs.Meminfo(); err != nil || len(mem) == 0 {
		t.Error(err)
	}
	if devs, err := fs.NetDev(); err != nil || len(devs) == 0 {
		t.Error(err)
	}
	if _, err := fs.Snmp(); err != nil {
		t.Error(err)
	}
	if _, err := fs.DefaultRouteInterfaces(); err != nil {
		t.Error(err)
	}
}
