// Sum a file byte buffer by packing every 8 bytes into one uint64_t word.

#include <stdint.h>

extern "C" {
void file_sum64(const unsigned char* file_data, uint32_t byte_count, uint64_t* sum_out) {
#pragma HLS INTERFACE m_axi port=file_data offset=slave bundle=gmem0
#pragma HLS INTERFACE m_axi port=sum_out offset=slave bundle=gmem1
#pragma HLS INTERFACE s_axilite port=file_data bundle=control
#pragma HLS INTERFACE s_axilite port=byte_count bundle=control
#pragma HLS INTERFACE s_axilite port=sum_out bundle=control
#pragma HLS INTERFACE s_axilite port=return bundle=control

    uint64_t sum = 0;
    const uint32_t word_count = (byte_count + 7) >> 3;

sum_loop:
    for (uint32_t i = 0; i < word_count; ++i) {
        uint64_t word = 0;

    pack_loop:
        for (uint32_t j = 0; j < 8; ++j) {
#pragma HLS UNROLL
            const uint32_t byte_index = (i << 3) + j;
            if (byte_index < byte_count) {
                word |= static_cast<uint64_t>(file_data[byte_index]) << (j << 3);
            }
        }

        sum += word;
    }

    sum_out[0] = sum;
}
}

