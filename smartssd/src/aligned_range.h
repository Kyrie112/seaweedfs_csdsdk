// Shared range geometry for the SmartSSD CSD agent.
//
// SeaweedFS needle payloads start at needleOffset + NeedleHeaderSize +
// DataSizeSize (+20) and are not padded, so a payload range is essentially
// never 512-byte aligned. A device that wants O_DIRECT must therefore read the
// enclosing aligned window and skip the leading bytes when packing the payload
// into uint64 words; the skip cannot be done by the host without copying the
// whole payload again through the P2P mapping.
//
// Everything here is header-only and free of XRT/OpenCL dependencies so the
// agent and the host-side reference tests share one definition.
#ifndef CSD_ALIGNED_RANGE_H
#define CSD_ALIGNED_RANGE_H

#include <cstddef>
#include <cstdint>
#include <unistd.h>

namespace csd {

// Logical block size of the NVMe devices used in this work, and the unit that
// SeaweedFS and the agent agree on for O_DIRECT reads.
constexpr int64_t kReadAlignment = 512;

struct AlignedWindow {
  int64_t read_offset = 0;
  int64_t read_size = 0;
  int64_t payload_shift = 0;
};

// plan_aligned_window returns the smallest kReadAlignment-aligned window that
// fully contains [data_offset, data_offset + data_size), together with the
// payload's byte shift inside that window.
inline AlignedWindow plan_aligned_window(int64_t data_offset, int64_t data_size,
                                         int64_t alignment = kReadAlignment) {
  AlignedWindow window;
  if (data_size <= 0) {
    window.read_offset = data_offset;
    window.read_size = 0;
    window.payload_shift = 0;
    return window;
  }
  int64_t shift = data_offset % alignment;
  if (shift < 0) {
    shift += alignment;
  }
  window.payload_shift = shift;
  window.read_offset = data_offset - shift;
  const int64_t span = shift + data_size;
  window.read_size = span;
  const int64_t remainder = span % alignment;
  if (remainder != 0) {
    window.read_size = span + (alignment - remainder);
  }
  return window;
}

// covers reports whether the window really contains the whole payload.
inline bool covers(const AlignedWindow& window, int64_t data_offset, int64_t data_size) {
  return window.read_size > 0 &&
         window.read_offset + window.payload_shift == data_offset &&
         window.payload_shift + data_size <= window.read_size;
}

// available_in_window returns how many bytes of the aligned window actually
// exist in a data file of the given size.
//
// The aligned window can run past the end of a volume .dat file: the file ends
// exactly at the last needle's last byte, and rounding the window up to a 512
// byte boundary then asks for more. The bytes past EOF are never part of the
// payload, so they must be tolerated rather than treated as a short read.
inline int64_t available_in_window(const AlignedWindow& window, int64_t file_size) {
  if (window.read_size <= 0) {
    return 0;
  }
  int64_t end = window.read_offset + window.read_size;
  if (file_size < end) {
    end = file_size;
  }
  const int64_t available = end - window.read_offset;
  return available > 0 ? available : 0;
}

// payload_within_available reports whether every payload byte is inside the
// part of the window that exists in the file. A false result means the needle
// itself is truncated and the request must fail.
inline bool payload_within_available(const AlignedWindow& window, int64_t data_size,
                                     int64_t available) {
  return window.payload_shift >= 0 && window.payload_shift + data_size <= available;
}

// software_sum_payload is the host-side reference model of the v2 FPGA kernel:
// pack bytes [payload_shift, payload_shift + payload_size) into little-endian
// uint64 words and add up the words.
inline uint64_t software_sum_payload(const uint8_t* buffer, int64_t buffer_len,
                                     int64_t payload_shift, int64_t payload_size) {
  if (payload_shift < 0) {
    payload_shift = 0;
  }
  if (payload_shift > buffer_len) {
    payload_shift = buffer_len;
  }
  if (payload_size < 0) {
    payload_size = 0;
  }
  if (payload_shift + payload_size > buffer_len) {
    payload_size = buffer_len - payload_shift;
  }

  uint64_t sum = 0;
  const int64_t word_count = (payload_size + 7) / 8;
  for (int64_t i = 0; i < word_count; ++i) {
    uint64_t word = 0;
    for (int64_t j = 0; j < 8; ++j) {
      const int64_t index = i * 8 + j;
      if (index < payload_size) {
        word |= static_cast<uint64_t>(buffer[payload_shift + index])
                << (j * 8);
      }
    }
    sum += word;
  }
  return sum;
}

// software_sum_legacy is the reference model of the v1 kernel, where the buffer
// starts exactly at the payload.
inline uint64_t software_sum_legacy(const uint8_t* buffer, int64_t byte_count) {
  return software_sum_payload(buffer, byte_count, 0, byte_count);
}

// read_fully_with loops until `count` bytes have been read. A single pread may
// legally return fewer bytes than requested; treating that as success is how a
// device silently computes over a partially filled buffer. The caller must
// compare the returned total against `count`.
template <typename PreadFn>
inline ssize_t read_fully_with(PreadFn pread_fn, void* buffer, size_t count,
                               int64_t offset) {
  auto* out = static_cast<uint8_t*>(buffer);
  size_t done = 0;
  while (done < count) {
    const ssize_t got =
        pread_fn(out + done, count - done, offset + static_cast<int64_t>(done));
    if (got < 0) {
      return -1;
    }
    if (got == 0) {
      break;  // EOF: the caller sees a short count and must reject it.
    }
    done += static_cast<size_t>(got);
  }
  return static_cast<ssize_t>(done);
}

inline ssize_t read_fully_at(int fd, void* buffer, size_t count, int64_t offset) {
  return read_fully_with(
      [fd](void* dst, size_t len, int64_t off) {
        return ::pread(fd, dst, len, static_cast<off_t>(off));
      },
      buffer, count, offset);
}

}  // namespace csd

#endif  // CSD_ALIGNED_RANGE_H
