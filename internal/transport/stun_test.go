package transport

import (
	"encoding/binary"
	"net"
	"slices"
	"testing"
	"time"
)

// RFC 5769 section 2.2: sample IPv4 response, mapped address 192.0.2.1:32853.
// MESSAGE-INTEGRITY and FINGERPRINT values are zeroed; we don't check them.
func TestParseSTUNVector(t *testing.T) {
	b := []byte{
		0x01, 0x01, 0x00, 0x3c, 0x21, 0x12, 0xa4, 0x42,
		0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae,
		0x80, 0x22, 0x00, 0x0b, 0x74, 0x65, 0x73, 0x74, 0x20, 0x76, 0x65, 0x63, 0x74, 0x6f, 0x72, 0x20,
		0x00, 0x20, 0x00, 0x08, 0x00, 0x01, 0xa1, 0x47, 0xe1, 0x12, 0xa6, 0x43,
		0x00, 0x08, 0x00, 0x14, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0x80, 0x28, 0x00, 0x04, 0, 0, 0, 0,
	}
	var tx [12]byte
	copy(tx[:], b[8:20])
	a, ok := parseSTUNResponse(b, tx)
	if !ok || a.String() != "192.0.2.1:32853" {
		t.Fatalf("got %v %v", a, ok)
	}
	tx[0] ^= 1
	if _, ok := parseSTUNResponse(b, tx); ok {
		t.Fatal("accepted a response for another transaction")
	}
	if isSTUN(ctlPacket(ctl{T: "regok"})) {
		t.Fatal("control packet mistaken for STUN")
	}
}

// fakeSTUN answers binding requests with a fixed mapped address.
func fakeSTUN(t *testing.T, mapped *net.UDPAddr) string {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < stunHeaderLen || binary.BigEndian.Uint16(buf[0:2]) != stunBindingRequest {
				continue
			}
			resp := make([]byte, stunHeaderLen+12)
			binary.BigEndian.PutUint16(resp[0:2], stunBindingSuccess)
			binary.BigEndian.PutUint16(resp[2:4], 12)
			binary.BigEndian.PutUint32(resp[4:8], stunCookie)
			copy(resp[8:20], buf[8:20])
			a := resp[20:]
			binary.BigEndian.PutUint16(a[0:2], stunXorMappedAddr)
			binary.BigEndian.PutUint16(a[2:4], 8)
			a[5] = stunFamilyIPv4
			binary.BigEndian.PutUint16(a[6:8], uint16(mapped.Port)^(stunCookie>>16))
			ip := mapped.IP.To4()
			var c [4]byte
			binary.BigEndian.PutUint32(c[:], stunCookie)
			for i := range 4 {
				a[8+i] = ip[i] ^ c[i]
			}
			conn.WriteToUDP(resp, from)
		}
	}()
	return conn.LocalAddr().String()
}

func TestSTUNCandidate(t *testing.T) {
	srv := startServer(t)
	stun := fakeSTUN(t, &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 4242})
	n := startNode(t, Config{Key: newKey(t), Server: srv, STUN: []string{stun}})
	deadline := time.Now().Add(3 * time.Second)
	for n.STUNMapped() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n.STUNMapped() != "198.51.100.7:4242" {
		t.Fatalf("STUN mapped %q", n.STUNMapped())
	}
	if !slices.Contains(n.candidates(), "198.51.100.7:4242") {
		t.Fatalf("candidates %v lack the STUN address", n.candidates())
	}
}
