package archive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// Minimal Ogg Opus (RFC 7845) muxer / demuxer: enough to store each
// utterance as a standard .ogg file that browsers and ASR APIs accept, and to
// read the raw packets back for replay.

var crcTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return
}()

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, x := range b {
		crc = crc<<8 ^ crcTable[byte(crc>>24)^x]
	}
	return crc
}

// PacketSamples returns the duration of an Opus packet in 48 kHz samples,
// derived from its TOC byte (RFC 6716 §3.1).
func PacketSamples(pkt []byte) int {
	if len(pkt) == 0 {
		return 0
	}
	toc := pkt[0]
	cfg := toc >> 3
	var per int // samples per frame at 48 kHz
	switch {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		per = []int{480, 960, 1920, 2880}[cfg%4]
	case cfg < 16: // Hybrid: 10, 20 ms
		per = []int{480, 960}[cfg%2]
	default: // CELT: 2.5, 5, 10, 20 ms
		per = []int{120, 240, 480, 960}[cfg%4]
	}
	frames := 1
	switch toc & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(pkt) < 2 {
			return 0
		}
		frames = int(pkt[1] & 0x3f)
	}
	return per * frames
}

type oggWriter struct {
	w       io.Writer
	serial  uint32
	seq     uint32
	granule int64
}

func (o *oggWriter) page(flags byte, granule int64, packet []byte) error {
	var segs []byte
	n := len(packet)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	if len(segs) > 255 {
		return errors.New("ogg: packet too large")
	}
	var hdr bytes.Buffer
	hdr.WriteString("OggS")
	hdr.WriteByte(0)
	hdr.WriteByte(flags)
	binary.Write(&hdr, binary.LittleEndian, granule)
	binary.Write(&hdr, binary.LittleEndian, o.serial)
	binary.Write(&hdr, binary.LittleEndian, o.seq)
	binary.Write(&hdr, binary.LittleEndian, uint32(0)) // crc placeholder
	hdr.WriteByte(byte(len(segs)))
	hdr.Write(segs)
	page := append(hdr.Bytes(), packet...)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
	o.seq++
	_, err := o.w.Write(page)
	return err
}

// EncodeOggOpus wraps raw Opus packets into an Ogg Opus stream.
func EncodeOggOpus(w io.Writer, sampleRate, channels int, packets [][]byte) error {
	// Granule positions include the pre-skip (RFC 7845 §4), else players
	// trim the last 312 samples of audio.
	o := &oggWriter{w: w, serial: 0x70747477, granule: 312} // "pttw"
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1
	head[9] = byte(channels)
	binary.LittleEndian.PutUint16(head[10:], 312) // pre-skip
	binary.LittleEndian.PutUint32(head[12:], uint32(sampleRate))
	start := o.granule
	o.granule = 0
	if err := o.page(0x02, 0, head); err != nil {
		return err
	}
	vendor := "ptt-walkie"
	tags := make([]byte, 8+4+len(vendor)+4)
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:], uint32(len(vendor)))
	copy(tags[12:], vendor)
	if err := o.page(0, 0, tags); err != nil {
		return err
	}
	o.granule = start
	for i, p := range packets {
		o.granule += int64(PacketSamples(p))
		flags := byte(0)
		if i == len(packets)-1 {
			flags = 0x04
		}
		if err := o.page(flags, o.granule, p); err != nil {
			return err
		}
	}
	return nil
}

// DecodeOggOpus extracts the audio packets (skipping OpusHead / OpusTags).
func DecodeOggOpus(r io.Reader) ([][]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var packets [][]byte
	var cur []byte
	for len(data) > 0 {
		if len(data) < 27 || string(data[:4]) != "OggS" {
			return nil, errors.New("ogg: bad page")
		}
		nseg := int(data[26])
		if len(data) < 27+nseg {
			return nil, errors.New("ogg: truncated")
		}
		segs := data[27 : 27+nseg]
		body := data[27+nseg:]
		for _, s := range segs {
			if len(body) < int(s) {
				return nil, errors.New("ogg: truncated")
			}
			cur = append(cur, body[:s]...)
			body = body[s:]
			if s < 255 {
				packets = append(packets, cur)
				cur = nil
			}
		}
		data = body
	}
	if len(packets) < 2 {
		return nil, errors.New("ogg: missing opus headers")
	}
	return packets[2:], nil
}
