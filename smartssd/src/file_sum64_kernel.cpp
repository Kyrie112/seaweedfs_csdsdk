// Sum a file byte range by packing every 8 bytes into one uint64_t word.
//
// v2 adds payload_shift / payload_size so a caller that had to read a
// block-aligned window (O_DIRECT) can still pack the payload from its own first
// byte. With payload_shift=0 and payload_size=byte_count this is bit-identical
// to the v1 kernel, which only supported buffers that start at the payload.
//
// SeaweedFS needle payloads start at needleOffset + 20 and are not padded, so a
// payload-aligned buffer is essentially never block aligned; without the shift
// the device cannot use O_DIRECT at all.

#include <stdint.h>

extern "C" {
void file_sum64(const unsigned char* file_data, uint32_t byte_count,
                uint32_t payload_shift, uint32_t payload_size,
                uint64_t* sum_out) {
#pragma HLS INTERFACE m_axi port=file_data offset=slave bundle=gmem0
#pragma HLS INTERFACE m_axi port=sum_out offset=slave bundle=gmem1
#pragma HLS INTERFACE s_axilite port=file_data bundle=control
#pragma HLS INTERFACE s_axilite port=byte_count bundle=control
#pragma HLS INTERFACE s_axilite port=payload_shift bundle=control
#pragma HLS INTERFACE s_axilite port=payload_size bundle=control
#pragma HLS INTERFACE s_axilite port=sum_out bundle=control
#pragma HLS INTERFACE s_axilite port=return bundle=control

    uint64_t sum = 0;

    // Never read outside the range the host declared. payload_shift is the
    // number of leading bytes of the aligned window that belong to the needle
    // header (or to the preceding needle) and must not be summed.
    if (payload_shift > byte_count) {
        payload_shift = byte_count;
    }
    if (payload_size > byte_count - payload_shift) {
        payload_size = byte_count - payload_shift;
    }

    const uint32_t word_count = (payload_size + 7) >> 3;

sum_loop:
    for (uint32_t i = 0; i < word_count; ++i) {
        uint64_t word = 0;

    pack_loop:
        for (uint32_t j = 0; j < 8; ++j) {
#pragma HLS UNROLL
            const uint32_t payload_index = (i << 3) + j;
            if (payload_index < payload_size) {
                word |= static_cast<uint64_t>(
                            file_data[payload_shift + payload_index])
                        << (j << 3);
            }
        }

        sum += word;
    }

    sum_out[0] = sum;
}
}
