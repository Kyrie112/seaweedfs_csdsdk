// HLS reference kernel for the near-storage image top-k operator.
//
// The agent currently emulates this operator on the host (see csd_agent.cpp),
// because synthesising takes a couple of hours; this file is the device-side
// implementation to synthesise next. The descriptor definition and the L1
// metric match src/image_topk.h exactly, so a synthesised kernel only has to be
// verified once against the host implementation.
//
// Synthesis:
//   v++ -c -t hw --platform <u2_gen3x4> -k imgtopk_scan --temp_dir _x.hw
//       -o _x.hw/imgtopk_scan.xo src/imgtopk_kernel.cpp

#include <stdint.h>

#define IMAGE_SIDE 64
#define IMAGE_RECORD_BYTES (IMAGE_SIDE * IMAGE_SIDE)
#define DESCRIPTOR_DIMS 68
#define IMGTOPK_MAX_K 64

extern "C" {
void imgtopk_scan(const unsigned char* records, uint32_t record_count,
                  const int32_t* query_descriptor, uint32_t k,
                  int64_t* out_index, int64_t* out_distance) {
#pragma HLS INTERFACE m_axi port=records offset=slave bundle=gmem0
#pragma HLS INTERFACE m_axi port=query_descriptor offset=slave bundle=gmem1
#pragma HLS INTERFACE m_axi port=out_index offset=slave bundle=gmem2
#pragma HLS INTERFACE m_axi port=out_distance offset=slave bundle=gmem3
#pragma HLS INTERFACE s_axilite port=records bundle=control
#pragma HLS INTERFACE s_axilite port=record_count bundle=control
#pragma HLS INTERFACE s_axilite port=query_descriptor bundle=control
#pragma HLS INTERFACE s_axilite port=k bundle=control
#pragma HLS INTERFACE s_axilite port=out_index bundle=control
#pragma HLS INTERFACE s_axilite port=out_distance bundle=control
#pragma HLS INTERFACE s_axilite port=return bundle=control

  if (k > IMGTOPK_MAX_K) {
    k = IMGTOPK_MAX_K;
  }

  int64_t best_index[IMGTOPK_MAX_K];
  int64_t best_distance[IMGTOPK_MAX_K];
#pragma HLS ARRAY_PARTITION variable=best_index complete dim=1
#pragma HLS ARRAY_PARTITION variable=best_distance complete dim=1

init_insert:
  for (uint32_t i = 0; i < IMGTOPK_MAX_K; ++i) {
    best_index[i] = 0;
    best_distance[i] = 0x7FFFFFFFFFFFFFFFLL;
  }

record_loop:
  for (uint32_t r = 0; r < record_count; ++r) {
    const unsigned char* record = records + (int64_t)r * IMAGE_RECORD_BYTES;
    int32_t descriptor[DESCRIPTOR_DIMS];
#pragma HLS ARRAY_PARTITION variable=descriptor complete dim=1

  init_descriptor:
    for (int i = 0; i < DESCRIPTOR_DIMS; ++i) {
#pragma HLS UNROLL
      descriptor[i] = 0;
    }

  pixel_loop:
    for (int y = 0; y < IMAGE_SIDE; ++y) {
    pixel_inner:
      for (int x = 0; x < IMAGE_SIDE; ++x) {
#pragma HLS PIPELINE II=1
        const int value = record[y * IMAGE_SIDE + x];
        descriptor[value >> 2] += 1;
        const int quadrant = ((y >= IMAGE_SIDE / 2) ? 2 : 0) +
                             ((x >= IMAGE_SIDE / 2) ? 1 : 0);
        descriptor[64 + quadrant] += value;
      }
    }

    int64_t distance = 0;
  distance_loop:
    for (int i = 0; i < DESCRIPTOR_DIMS; ++i) {
#pragma HLS UNROLL
      const int64_t delta =
          (int64_t)descriptor[i] - (int64_t)query_descriptor[i];
      distance += (delta < 0) ? -delta : delta;
    }

    // Insertion into a small sorted top-k held in registers: shift right every
    // entry that ranks worse than the new hit, then place it.
    if (distance < best_distance[k - 1] ||
        (distance == best_distance[k - 1] && (int64_t)r < best_index[k - 1])) {
      int slot = (int)k - 1;
    insert_loop:
      for (int i = (int)k - 1; i > 0; --i) {
#pragma HLS UNROLL
        if (best_distance[i - 1] > distance ||
            (best_distance[i - 1] == distance && best_index[i - 1] > (int64_t)r)) {
          best_distance[i] = best_distance[i - 1];
          best_index[i] = best_index[i - 1];
          slot = i - 1;
        }
      }
      best_distance[slot] = distance;
      best_index[slot] = r;
    }
  }

write_out:
  for (uint32_t i = 0; i < k; ++i) {
#pragma HLS UNROLL
    out_index[i] = best_index[i];
    out_distance[i] = best_distance[i];
  }
}
}
