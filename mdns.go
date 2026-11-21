package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
)

const (
	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
)

type rr struct {
	name string
	typ  uint16
	ptr  string   // PTR target
	port uint16   // SRV
	tgt  string   // SRV target
	txt  []string // TXT strings
	ip   net.IP   // A
}

type service struct {
	instance string
	host     string
	ip       net.IP
	port     int
	txt      []string // ordered key=value strings
}

func (s *service) txtGet(key string) string {
	for _, t := range s.txt {
		if k, v, ok := strings.Cut(t, "="); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func encodeName(n string) []byte {
	var b []byte
	for _, l := range strings.Split(strings.TrimSuffix(n, "."), ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

func buildQuery(name string, qtype uint16) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[4:], 1)
	b = append(b, encodeName(name)...)
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, 1)
}

func readName(b []byte, off int) (string, int, error) {
	var parts []string
	end := -1
	for jumps := 0; jumps < 50; jumps++ {
		if off >= len(b) {
			return "", 0, errors.New("name out of range")
		}
		l := int(b[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.Join(parts, "."), end, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return "", 0, errors.New("bad pointer")
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
		default:
			if off+1+l > len(b) {
				return "", 0, errors.New("label out of range")
			}
			parts = append(parts, string(b[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, errors.New("too many jumps")
}

func parseDNS(b []byte) ([]rr, error) {
	if len(b) < 12 {
		return nil, errors.New("short packet")
	}
	qd := int(binary.BigEndian.Uint16(b[4:]))
	n := int(binary.BigEndian.Uint16(b[6:])) + int(binary.BigEndian.Uint16(b[8:])) + int(binary.BigEndian.Uint16(b[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, o, err := readName(b, off)
		if err != nil {
			return nil, err
		}
		off = o + 4
	}
	var out []rr
	for i := 0; i < n; i++ {
		name, o, err := readName(b, off)
		if err != nil || o+10 > len(b) {
			return out, err
		}
		r := rr{name: name, typ: binary.BigEndian.Uint16(b[o:])}
		rdl := int(binary.BigEndian.Uint16(b[o+8:]))
		rd := o + 10
		if rd+rdl > len(b) {
			return out, errors.New("rdata out of range")
		}
		switch r.typ {
		case typePTR:
			r.ptr, _, _ = readName(b, rd)
		case typeSRV:
			if rdl >= 7 {
				r.port = binary.BigEndian.Uint16(b[rd+4:])
				r.tgt, _, _ = readName(b, rd+6)
			}
		case typeTXT:
			for j := rd; j < rd+rdl; {
				l := int(b[j])
				if j+1+l > rd+rdl {
					break
				}
				if l > 0 {
					r.txt = append(r.txt, string(b[j+1:j+1+l]))
				}
				j += 1 + l
			}
		case typeA:
			if rdl == 4 {
				r.ip = net.IP(append([]byte(nil), b[rd:rd+4]...))
			}
		}
		out = append(out, r)
		off = rd + rdl
	}
	return out, nil
}

// mdnsQuery sends legacy-unicast mDNS queries on the given interface and gathers
// every record received within the timeout.
func mdnsQuery(ifname string, qs [][2]any, timeout time.Duration) ([]rr, error) {
	var ifaddr net.IP
	if ifname != "" {
		ifi, err := net.InterfaceByName(ifname)
		if err != nil {
			return nil, fmt.Errorf("interface %s: %w", ifname, err)
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				ifaddr = ipn.IP.To4()
				break
			}
		}
		if ifaddr == nil {
			return nil, fmt.Errorf("interface %s has no IPv4 address", ifname)
		}
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			if ifaddr != nil {
				var a [4]byte
				copy(a[:], ifaddr)
				serr = syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, a)
			}
		})
		return serr
	}}
	pc, err := lc.ListenPacket(nil, "udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	dst := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	for _, q := range qs {
		if _, err := pc.WriteTo(buildQuery(q[0].(string), q[1].(uint16)), dst); err != nil {
			return nil, err
		}
	}
	var all []rr
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 9000)
	for {
		pc.SetReadDeadline(deadline)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		rs, _ := parseDNS(buf[:n])
		all = append(all, rs...)
	}
	return all, nil
}

// discoverIPP browses _ipp._tcp on ifname and resolves every instance found.
func discoverIPP(ifname string) ([]service, error) {
	rs, err := mdnsQuery(ifname, [][2]any{{"_ipp._tcp.local", uint16(typePTR)}}, 2*time.Second)
	if err != nil {
		return nil, err
	}
	insts := map[string]bool{}
	for _, r := range rs {
		if r.typ == typePTR && strings.EqualFold(r.name, "_ipp._tcp.local") {
			insts[r.ptr] = true
		}
	}
	// Ask for SRV/TXT of each instance, plus whatever came as additional records.
	var qs [][2]any
	for in := range insts {
		qs = append(qs, [2]any{in, uint16(typeSRV)}, [2]any{in, uint16(typeTXT)})
	}
	if len(qs) > 0 {
		more, _ := mdnsQuery(ifname, qs, 2*time.Second)
		rs = append(rs, more...)
	}
	srv := map[string]rr{}
	txt := map[string][]string{}
	ips := map[string]net.IP{}
	for _, r := range rs {
		switch r.typ {
		case typeSRV:
			srv[strings.ToLower(r.name)] = r
		case typeTXT:
			txt[strings.ToLower(r.name)] = r.txt
		case typeA:
			ips[strings.ToLower(r.name)] = r.ip
		}
	}
	var hosts [][2]any
	for in := range insts {
		if s, ok := srv[strings.ToLower(in)]; ok && ips[strings.ToLower(s.tgt)] == nil {
			hosts = append(hosts, [2]any{s.tgt, uint16(typeA)})
		}
	}
	if len(hosts) > 0 {
		more, _ := mdnsQuery(ifname, hosts, 2*time.Second)
		for _, r := range more {
			if r.typ == typeA {
				ips[strings.ToLower(r.name)] = r.ip
			}
		}
	}
	var out []service
	for in := range insts {
		s, ok := srv[strings.ToLower(in)]
		if !ok {
			continue
		}
		label := strings.TrimSuffix(in, "._ipp._tcp.local")
		out = append(out, service{instance: unescapeDNS(label), host: s.tgt, ip: ips[strings.ToLower(s.tgt)],
			port: int(s.port), txt: txt[strings.ToLower(in)]})
	}
	return out, nil
}

// unescapeDNS turns "\032" style escapes into characters (names are stored raw here, so this is a no-op safety net).
func unescapeDNS(s string) string { return s }
