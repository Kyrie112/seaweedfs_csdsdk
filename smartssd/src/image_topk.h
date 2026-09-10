// Near-storage top-k image similarity search.
//
// The gallery is a concatenation of fixed-size grayscale thumbnails. The
// descriptor is integer-only on purpose so an FPGA kernel can build it in a
// single streaming pass over the record:
//
//   descriptor[g] = sum of pixel values in the 8x8 cell g of an 8x8 grid
//
// Similarity is the L1 distance between descriptors (smaller is closer).
// This header is free of XRT/OpenCL dependencies so it can be unit tested on
// the host and shared with the agent.
#ifndef CSD_IMAGE_TOPK_H
#define CSD_IMAGE_TOPK_H

#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <stdexcept>
#include <string>
#include <vector>

namespace csd {

constexpr int kImageSide = 64;
constexpr int kImageRecordSize = kImageSide * kImageSide;  // 4096 bytes
constexpr int kDescriptorDims = 64;

// image_descriptor computes the descriptor of one record.
inline void image_descriptor(const uint8_t* record, int32_t* out) {
  for (int i = 0; i < kDescriptorDims; ++i) {
    out[i] = 0;
  }
  for (int y = 0; y < kImageSide; ++y) {
    const int row = (y / 8) * 8;
    for (int x = 0; x < kImageSide; ++x) {
      out[row + x / 8] += record[y * kImageSide + x];
    }
  }
}

inline int64_t descriptor_distance(const int32_t* a, const int32_t* b) {
  int64_t distance = 0;
  for (int i = 0; i < kDescriptorDims; ++i) {
    int64_t delta = static_cast<int64_t>(a[i]) - static_cast<int64_t>(b[i]);
    if (delta < 0) {
      delta = -delta;
    }
    distance += delta;
  }
  return distance;
}

// decode_base64 reverses the standard base64 encoding used to ship the query
// descriptor. It is a dependency-free decoder because the agent deliberately
// avoids a JSON/base64 library.
inline std::vector<uint8_t> decode_base64(const std::string& text) {
  static const std::string alphabet =
      "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  int8_t table[256];
  for (int i = 0; i < 256; ++i) {
    table[i] = -1;
  }
  for (std::size_t i = 0; i < alphabet.size(); ++i) {
    table[static_cast<unsigned char>(alphabet[i])] = static_cast<int8_t>(i);
  }

  std::vector<uint8_t> out;
  out.reserve(text.size() * 3 / 4 + 3);
  uint32_t buffer = 0;
  int bits = 0;
  for (char c : text) {
    if (c == '=' || c == '\n' || c == '\r') {
      continue;
    }
    const int8_t value = table[static_cast<unsigned char>(c)];
    if (value < 0) {
      throw std::runtime_error("invalid base64 in query descriptor");
    }
    buffer = (buffer << 6) | static_cast<uint32_t>(value);
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out.push_back(static_cast<uint8_t>((buffer >> bits) & 0xFF));
    }
  }
  return out;
}

// decode_query_descriptor turns the base64 payload into kDescriptorDims int32
// values (little endian, matching the encoder used by the client tooling).
inline void decode_query_descriptor(const std::string& encoded, int32_t* out) {
  const std::vector<uint8_t> raw = decode_base64(encoded);
  if (raw.size() != static_cast<std::size_t>(kDescriptorDims) * 4) {
    throw std::runtime_error("query descriptor has " + std::to_string(raw.size()) +
                             " bytes, want " + std::to_string(kDescriptorDims * 4));
  }
  for (int i = 0; i < kDescriptorDims; ++i) {
    const uint32_t value = static_cast<uint32_t>(raw[4 * i]) |
                           (static_cast<uint32_t>(raw[4 * i + 1]) << 8) |
                           (static_cast<uint32_t>(raw[4 * i + 2]) << 16) |
                           (static_cast<uint32_t>(raw[4 * i + 3]) << 24);
    out[i] = static_cast<int32_t>(value);
  }
}

struct TopKHit {
  int64_t index = 0;
  int64_t distance = 0;
};

// image_topk scans every *complete* record inside the logical range
// [base, base + size) and returns the best k as "index:distance" pairs ordered
// by ascending distance.
//
// Records that straddle the range boundary are skipped: the filer only accepts
// files whose chunks tile the file, so each record is fully covered by exactly
// one chunk - the one that contains it in full. This is what makes the operator
// safe to run per chunk while keeping the global answer exact.
inline std::string image_topk(const uint8_t* payload, int64_t base, int64_t size,
                              int64_t record_size, const int32_t* query, int k) {
  if (record_size <= 0 || k <= 0) {
    throw std::runtime_error("invalid record size or k");
  }
  std::vector<TopKHit> best;
  best.reserve(static_cast<std::size_t>(k));

  const int64_t end = base + size;
  int64_t offset = ((base + record_size - 1) / record_size) * record_size;
  for (; offset + record_size <= end; offset += record_size) {
    const int64_t local = offset - base;
    int32_t descriptor[kDescriptorDims];
    image_descriptor(payload + local, descriptor);
    const TopKHit hit{offset / record_size, descriptor_distance(query, descriptor)};

    if (static_cast<int>(best.size()) == k &&
        (hit.distance > best.back().distance ||
         (hit.distance == best.back().distance && hit.index > best.back().index))) {
      continue;
    }
    std::size_t position = best.size();
    while (position > 0 &&
           (best[position - 1].distance > hit.distance ||
            (best[position - 1].distance == hit.distance &&
             best[position - 1].index > hit.index))) {
      --position;
    }
    best.insert(best.begin() + static_cast<std::ptrdiff_t>(position), hit);
    if (static_cast<int>(best.size()) > k) {
      best.pop_back();
    }
  }

  std::string out;
  for (std::size_t i = 0; i < best.size(); ++i) {
    if (i > 0) {
      out += ',';
    }
    out += std::to_string(best[i].index) + ":" + std::to_string(best[i].distance);
  }
  return out;
}

}  // namespace csd

#endif  // CSD_IMAGE_TOPK_H
