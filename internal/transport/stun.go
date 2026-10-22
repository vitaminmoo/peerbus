package transport

import (
	"crypto/rand"
	"encoding/binary"
	"net"
)

// Minimal STUN (RFC 8489) binding client. The rendezvous server already
// reports our address, but a session behind the same NAT as the server only
// sees the router's hairpin address; a STUN server outside that NAT reports
// the real public mapping, which is what remote peers need to punch to.

const (
	stunCookie              = 0x2112A442
	stunBindingRequest      = 0x0001
	stunBindingSuccess      = 0x0101
	stunXorMappedAddr       = 0x0020
	stunMappedAddr          = 0x0001
	stunHeaderLen           = 20
	stunFamilyIPv4     byte = 0x01
)

func isSTUN(b []byte) bool {
	return len(b) >= stunHeaderLen && b[0] < 0x40 && binary.BigEndian.Uint32(b[4:8]) == stunCookie
}

func stunRequest() (pkt []byte, txid [12]byte) {
	rand.Read(txid[:])
	pkt = make([]byte, stunHeaderLen)
	binary.BigEndian.PutUint16(pkt[0:2], stunBindingRequest)
	binary.BigEndian.PutUint32(pkt[4:8], stunCookie)
	copy(pkt[8:20], txid[:])
	return pkt, txid
}

// parseSTUNResponse returns the mapped IPv4 address from a binding success
// response carrying the given transaction id.
func parseSTUNResponse(b []byte, txid [12]byte) (*net.UDPAddr, bool) {
	if !isSTUN(b) || binary.BigEndian.Uint16(b[0:2]) != stunBindingSuccess || string(b[8:20]) != string(txid[:]) {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if stunHeaderLen+n > len(b) {
		return nil, false
	}
	attrs := b[stunHeaderLen : stunHeaderLen+n]
	var fallback *net.UDPAddr
	for len(attrs) >= 4 {
		typ := binary.BigEndian.Uint16(attrs[0:2])
		l := int(binary.BigEndian.Uint16(attrs[2:4]))
		if 4+l > len(attrs) {
			break
		}
		v := attrs[4 : 4+l]
		if l >= 8 && v[1] == stunFamilyIPv4 {
			port := binary.BigEndian.Uint16(v[2:4])
			ip := net.IPv4(v[4], v[5], v[6], v[7])
			switch typ {
			case stunXorMappedAddr:
				port ^= stunCookie >> 16
				var c [4]byte
				binary.BigEndian.PutUint32(c[:], stunCookie)
				ip = net.IPv4(v[4]^c[0], v[5]^c[1], v[6]^c[2], v[7]^c[3])
				return &net.UDPAddr{IP: ip, Port: int(port)}, true
			case stunMappedAddr:
				fallback = &net.UDPAddr{IP: ip, Port: int(port)}
			}
		}
		attrs = attrs[4+(l+3)&^3:] // attributes are padded to 4 bytes
	}
	return fallback, fallback != nil
}
