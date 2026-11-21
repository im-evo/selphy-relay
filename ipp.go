package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

type attr struct {
	tag   byte
	name  string
	value []byte
}

// msg is a parsed IPP message. gs holds groups of attributes: either a single
// delimiter tag, or a named attribute followed by its nameless continuation values.
type msg struct {
	head [8]byte
	gs   [][]attr
	data []byte
}

func parseIPP(b []byte) (*msg, error) {
	if len(b) < 9 {
		return nil, errors.New("message too short")
	}
	m := &msg{}
	copy(m.head[:], b[:8])
	i := 8
	for i < len(b) {
		t := b[i]
		i++
		if t < 0x10 {
			m.gs = append(m.gs, []attr{{tag: t}})
			if t == 0x03 {
				m.data = b[i:]
				return m, nil
			}
			continue
		}
		if i+2 > len(b) {
			return nil, errors.New("truncated name length")
		}
		nl := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		if i+nl+2 > len(b) {
			return nil, errors.New("truncated name")
		}
		n := string(b[i : i+nl])
		i += nl
		vl := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		if i+vl > len(b) {
			return nil, errors.New("truncated value")
		}
		v := append([]byte(nil), b[i:i+vl]...)
		i += vl
		a := attr{t, n, v}
		last := len(m.gs) - 1
		if n == "" && last >= 0 && m.gs[last][0].tag >= 0x10 {
			m.gs[last] = append(m.gs[last], a)
		} else {
			m.gs = append(m.gs, []attr{a})
		}
	}
	return nil, errors.New("missing end-of-attributes tag")
}

func (m *msg) encode() []byte {
	var buf bytes.Buffer
	buf.Write(m.head[:])
	for _, g := range m.gs {
		for _, a := range g {
			buf.WriteByte(a.tag)
			if a.tag < 0x10 {
				continue
			}
			binary.Write(&buf, binary.BigEndian, uint16(len(a.name)))
			buf.WriteString(a.name)
			binary.Write(&buf, binary.BigEndian, uint16(len(a.value)))
			buf.Write(a.value)
		}
	}
	buf.Write(m.data)
	return buf.Bytes()
}

func (m *msg) op() uint16        { return binary.BigEndian.Uint16(m.head[2:4]) }
func (m *msg) setOp(op uint16)   { binary.BigEndian.PutUint16(m.head[2:4], op) }
func (m *msg) status() uint16    { return binary.BigEndian.Uint16(m.head[2:4]) }
func (m *msg) reqID() uint32     { return binary.BigEndian.Uint32(m.head[4:8]) }
func (m *msg) setReqID(i uint32) { binary.BigEndian.PutUint32(m.head[4:8], i) }

func (m *msg) find(name string) int {
	for i, g := range m.gs {
		if g[0].tag >= 0x10 && g[0].name == name {
			return i
		}
	}
	return -1
}

func (m *msg) str(name string) string {
	if i := m.find(name); i >= 0 {
		return string(m.gs[i][0].value)
	}
	return ""
}

func (m *msg) strs(name string) []string {
	i := m.find(name)
	if i < 0 {
		return nil
	}
	var out []string
	for _, a := range m.gs[i] {
		if a.tag == 0x34 || a.tag == 0x37 || a.tag == 0x4a {
			continue
		}
		out = append(out, fmtValue(a))
	}
	return out
}

func (m *msg) int(name string) (int, bool) {
	i := m.find(name)
	if i < 0 || len(m.gs[i][0].value) != 4 {
		return 0, false
	}
	return int(int32(binary.BigEndian.Uint32(m.gs[i][0].value))), true
}

func (m *msg) remove(name string) {
	if i := m.find(name); i >= 0 {
		m.gs = append(m.gs[:i], m.gs[i+1:]...)
	}
}

// setOpAttr sets (or adds to the operation-attributes group) a single-valued attribute.
func (m *msg) setOpAttr(tag byte, name string, value []byte) {
	if i := m.find(name); i >= 0 {
		m.gs[i] = []attr{{tag, name, value}}
		return
	}
	for i, g := range m.gs {
		if g[0].tag == 0x01 {
			j := i + 1
			for j < len(m.gs) && m.gs[j][0].tag >= 0x10 {
				j++
			}
			m.gs = append(m.gs[:j], append([][]attr{{{tag, name, value}}}, m.gs[j:]...)...)
			return
		}
	}
}

func fmtValue(a attr) string {
	switch a.tag {
	case 0x21, 0x23:
		if len(a.value) == 4 {
			return fmt.Sprint(int32(binary.BigEndian.Uint32(a.value)))
		}
	case 0x22:
		if len(a.value) == 1 {
			return fmt.Sprint(a.value[0] != 0)
		}
	}
	return string(a.value)
}

func ippAttr(tag byte, name, value string) []byte {
	var b bytes.Buffer
	b.WriteByte(tag)
	binary.Write(&b, binary.BigEndian, uint16(len(name)))
	b.WriteString(name)
	binary.Write(&b, binary.BigEndian, uint16(len(value)))
	b.WriteString(value)
	return b.Bytes()
}

// buildGPA builds a Get-Printer-Attributes request for the given attributes.
func buildGPA(uri string, attrs ...string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x02, 0x00, 0x00, 0x0b, 0, 0, 0, 1, 0x01})
	b.Write(ippAttr(0x47, "attributes-charset", "utf-8"))
	b.Write(ippAttr(0x48, "attributes-natural-language", "en"))
	b.Write(ippAttr(0x45, "printer-uri", uri))
	for i, a := range attrs {
		n := ""
		if i == 0 {
			n = "requested-attributes"
		}
		b.Write(ippAttr(0x44, n, a))
	}
	b.WriteByte(0x03)
	return b.Bytes()
}

var opNames = map[uint16]string{2: "Print-Job", 4: "Validate-Job", 5: "Create-Job", 6: "Send-Document",
	8: "Cancel-Job", 9: "Get-Job-Attributes", 10: "Get-Jobs", 11: "Get-Printer-Attributes", 0x3c: "Identify-Printer"}

func opName(op uint16) string {
	if s, ok := opNames[op]; ok {
		return s
	}
	return fmt.Sprintf("op-0x%04x", op)
}
