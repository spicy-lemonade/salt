package keys

import (
	"fmt"
	"strings"
)

// bech32Encode implements BIP 173 encoding, which age uses for keys. age does
// not export a way to build an identity from raw bytes, so salt encodes the
// derived scalar and hands the string to age.ParseX25519Identity, which
// validates it.
func bech32Encode(hrp string, data []byte) (string, error) {
	values, err := convertBits(data, 8, 5)
	if err != nil {
		return "", err
	}
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range values {
		sb.WriteByte(charset[v])
	}
	for _, v := range bech32Checksum(hrp, values) {
		sb.WriteByte(charset[v])
	}
	return sb.String(), nil
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32Checksum(hrp string, data []byte) []byte {
	values := make([]byte, 0, len(hrp)*2+1+len(data)+6)
	for i := 0; i < len(hrp); i++ {
		values = append(values, hrp[i]>>5)
	}
	values = append(values, 0)
	for i := 0; i < len(hrp); i++ {
		values = append(values, hrp[i]&31)
	}
	values = append(values, data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ 1
	out := make([]byte, 6)
	for i := range out {
		out[i] = byte(mod>>uint(5*(5-i))) & 31
	}
	return out
}

func convertBits(data []byte, from, to uint) ([]byte, error) {
	var acc uint32
	var bits uint
	var out []byte
	maxv := uint32(1)<<to - 1
	for _, b := range data {
		if uint32(b)>>from != 0 {
			return nil, fmt.Errorf("invalid data byte %d", b)
		}
		acc = acc<<from | uint32(b)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(to-bits)&maxv))
	}
	return out, nil
}
