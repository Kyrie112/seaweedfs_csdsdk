package storage

import "testing"

// TestComputeAlignedWindowProperties checks the invariant a device-side
// O_DIRECT read depends on: the window is block aligned, it fully contains the
// payload, the reported shift is exactly the payload offset inside the window,
// and the window is the *smallest* such aligned window.
func TestComputeAlignedWindowProperties(t *testing.T) {
	cases := []struct {
		name       string
		dataOffset int64
		dataSize   int64
	}{
		{"already aligned", 4096, 512},
		{"needle header offset", 255348, 8388608},
		{"tiny payload at unaligned start", 513, 1},
		{"unaligned start and size", 1023, 511},
		{"single byte at offset zero", 0, 1},
		{"exactly one block", 20, 492},
		{"one byte past a block", 20, 493},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readOffset, readSize, payloadShift := ComputeAlignedWindow(tc.dataOffset, tc.dataSize)

			if readOffset%CSDReadAlignment != 0 {
				t.Fatalf("readOffset %d is not %d-byte aligned", readOffset, CSDReadAlignment)
			}
			if readSize%CSDReadAlignment != 0 {
				t.Fatalf("readSize %d is not a multiple of %d", readSize, CSDReadAlignment)
			}
			if payloadShift != tc.dataOffset-readOffset {
				t.Fatalf("payloadShift = %d, want %d", payloadShift, tc.dataOffset-readOffset)
			}
			if payloadShift < 0 || payloadShift >= CSDReadAlignment {
				t.Fatalf("payloadShift %d outside [0,%d)", payloadShift, CSDReadAlignment)
			}
			if readOffset > tc.dataOffset {
				t.Fatalf("window starts at %d, after payload start %d", readOffset, tc.dataOffset)
			}
			if readOffset+readSize < tc.dataOffset+tc.dataSize {
				t.Fatalf("window [%d,%d) does not cover payload [%d,%d)",
					readOffset, readOffset+readSize, tc.dataOffset, tc.dataOffset+tc.dataSize)
			}
			// Minimality: neither edge can be pulled in by a further block.
			span := payloadShift + tc.dataSize
			if readSize != span && readSize-CSDReadAlignment >= span {
				t.Fatalf("window %d is larger than the minimal window over span %d", readSize, span)
			}
		})
	}
}

// TestComputeAlignedWindowKeepsPayloadPackable documents the property that makes
// the aligned path equivalent to a payload-aligned read: the payload starts at
// ReadOffset+PayloadShift, so an engine that skips PayloadShift bytes packs the
// payload from its first byte exactly as an unaligned read would.
func TestComputeAlignedWindowKeepsPayloadPackable(t *testing.T) {
	const needleHeader = 20 // NeedleHeaderSize + DataSizeSize
	for needleOffset := int64(0); needleOffset < 4096; needleOffset += 37 {
		dataOffset := needleOffset + needleHeader
		const dataSize = int64(600)

		readOffset, readSize, payloadShift := ComputeAlignedWindow(dataOffset, dataSize)
		if got := readOffset + payloadShift; got != dataOffset {
			t.Fatalf("needleOffset %d: readOffset+payloadShift = %d, want dataOffset %d",
				needleOffset, got, dataOffset)
		}
		if payloadShift+dataSize > readSize {
			t.Fatalf("needleOffset %d: payload [%d,%d) exceeds window size %d",
				needleOffset, payloadShift, payloadShift+dataSize, readSize)
		}
		// The classic SeaweedFS layout must never accidentally look aligned;
		// if it ever did, this test would stop proving anything.
		if needleOffset%CSDReadAlignment == 0 && payloadShift != needleHeader {
			t.Fatalf("needleOffset %d: expected shift %d, got %d", needleOffset, needleHeader, payloadShift)
		}
	}
}
