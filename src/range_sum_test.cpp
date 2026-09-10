// Host-side reference test for the aligned read window and the v1/v2 kernel
// sum semantics.
//
// It needs neither XRT nor an FPGA, so it runs on a workstation and in CI. It
// is what proves the alignment fix is semantically exact *before* the xclbin is
// re-synthesised: the v2 kernel is only correct if reading the enclosing
// 512-byte window and skipping payload_shift bytes yields bit-identical results
// to packing a buffer that starts at the payload.
#include "aligned_range.h"

#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <fcntl.h>
#include <string>
#include <unistd.h>
#include <vector>

namespace {

int failures = 0;
int checks = 0;

void check(bool condition, const std::string& what) {
  ++checks;
  if (!condition) {
    ++failures;
    std::fprintf(stderr, "FAIL: %s\n", what.c_str());
  }
}

std::vector<uint8_t> make_pattern(size_t size) {
  std::vector<uint8_t> data(size);
  uint32_t state = 0x12345678u;
  for (size_t i = 0; i < size; ++i) {
    state = state * 1664525u + 1013904223u;
    data[i] = static_cast<uint8_t>(state >> 24);
  }
  return data;
}

}  // namespace

int main() {
  const std::vector<uint8_t> file = make_pattern(1 << 20);

  // 1. Window geometry: aligned, covering, minimal, consistent shift.
  const int64_t needle_offsets[] = {0, 20, 37, 512, 4096, 255328};
  const int64_t payload_sizes[] = {1, 7, 8, 511, 512, 513, 4096, 8195};
  for (int64_t needle_offset : needle_offsets) {
    for (int64_t payload_size : payload_sizes) {
      const int64_t data_offset = needle_offset + 20;  // header + data size
      const csd::AlignedWindow window =
          csd::plan_aligned_window(data_offset, payload_size, csd::kReadAlignment);
      const std::string label = "needle=" + std::to_string(needle_offset) +
                                " size=" + std::to_string(payload_size);

      check(window.read_offset % csd::kReadAlignment == 0,
            label + ": read_offset is not block aligned");
      check(window.read_size % csd::kReadAlignment == 0,
            label + ": read_size is not a block multiple");
      check(window.payload_shift >= 0 && window.payload_shift < csd::kReadAlignment,
            label + ": payload_shift outside [0,alignment)");
      check(csd::covers(window, data_offset, payload_size),
            label + ": window does not cover the payload");
      check(window.read_offset + window.read_size <= static_cast<int64_t>(file.size()),
            label + ": window runs past the synthetic file");

      const int64_t span = window.payload_shift + payload_size;
      check(window.read_size == span ||
                window.read_size - csd::kReadAlignment < span,
            label + ": window is larger than the minimal aligned window");
    }
  }

  // 2. Semantic equivalence: reading the aligned window with the v2 kernel must
  //    match packing a buffer that starts at the payload.
  for (int64_t needle_offset : needle_offsets) {
    for (int64_t payload_size : payload_sizes) {
      const int64_t data_offset = needle_offset + 20;
      const csd::AlignedWindow window =
          csd::plan_aligned_window(data_offset, payload_size, csd::kReadAlignment);
      const std::string label = "needle=" + std::to_string(needle_offset) +
                                " size=" + std::to_string(payload_size);

      const std::vector<uint8_t> aligned(
          file.begin() + window.read_offset,
          file.begin() + window.read_offset + window.read_size);
      const uint64_t via_window = csd::software_sum_payload(
          aligned.data(), window.read_size, window.payload_shift, payload_size);

      const std::vector<uint8_t> payload(file.begin() + data_offset,
                                         file.begin() + data_offset + payload_size);
      const uint64_t via_payload =
          csd::software_sum_legacy(payload.data(), payload_size);

      check(via_window == via_payload,
            label + ": aligned-window sum differs from payload-aligned sum");
    }
  }

  // 3. v1 compatibility: with shift 0 and size == buffer length the v2 model
  //    must reproduce the v1 model exactly, so an existing xclbin stays usable.
  for (int64_t size : {1, 7, 8, 63, 512, 513, 4096, 8195}) {
    const uint64_t v1 = csd::software_sum_legacy(file.data(), size);
    const uint64_t v2 = csd::software_sum_payload(file.data(), size, 0, size);
    check(v1 == v2, "size=" + std::to_string(size) + ": v1/v2 models disagree");
  }

  // 4. Regression evidence: this is the defect the v2 kernel exists to avoid.
  //    Summing the aligned window as if the payload started at byte 0 changes
  //    the uint64 packing whenever the shift is not a multiple of 8.
  {
    const int64_t data_offset = 0 + 20;  // a real needle layout
    const int64_t payload_size = 4096;
    const csd::AlignedWindow window =
        csd::plan_aligned_window(data_offset, payload_size, csd::kReadAlignment);
    check(window.payload_shift % 8 != 0,
          "expected the needle layout to produce a non-word-aligned shift");

    const std::vector<uint8_t> aligned(
        file.begin() + window.read_offset,
        file.begin() + window.read_offset + window.read_size);
    const uint64_t naive =
        csd::software_sum_legacy(aligned.data(), window.read_size);
    const std::vector<uint8_t> payload(file.begin() + data_offset,
                                       file.begin() + data_offset + payload_size);
    const uint64_t reference =
        csd::software_sum_legacy(payload.data(), payload_size);
    check(naive != reference,
          "a naive aligned read is expected to change the result (shift=" +
              std::to_string(window.payload_shift) + ")");
  }

  // 5. Short reads must be reassembled, and a genuine short count must be
  //    visible to the caller instead of silently truncating the computation.
  {
    const std::vector<uint8_t> source = make_pattern(1000);
    auto dribble = [&source](void* dst, size_t len, int64_t off) -> ssize_t {
      if (off < 0 || static_cast<size_t>(off) >= source.size()) {
        return 0;
      }
      const size_t available = source.size() - static_cast<size_t>(off);
      const size_t chunk = std::min(len, std::min<size_t>(available, 100));
      std::memcpy(dst, source.data() + off, chunk);
      return static_cast<ssize_t>(chunk);
    };

    std::vector<uint8_t> dst(source.size(), 0);
    check(csd::read_fully_with(dribble, dst.data(), source.size(), 0) ==
              static_cast<ssize_t>(source.size()),
          "short reads were not reassembled to the full length");
    check(std::memcmp(dst.data(), source.data(), source.size()) == 0,
          "reassembled short reads do not match the source bytes");

    const ssize_t past_eof = csd::read_fully_with(dribble, dst.data(), 1200, 0);
    check(past_eof == 1000,
          "reading past EOF must report a short count, got " +
              std::to_string(past_eof));
  }

  // 6. The same loop against a real file descriptor.
  {
    char path[] = "/tmp/csd_range_sum_XXXXXX";
    const int fd = ::mkstemp(path);
    check(fd >= 0, "mkstemp failed");
    if (fd >= 0) {
      const std::vector<uint8_t> source = make_pattern(4096);
      check(::write(fd, source.data(), source.size()) ==
                static_cast<ssize_t>(source.size()),
            "failed to write the temporary file");

      std::vector<uint8_t> dst(source.size(), 0);
      check(csd::read_fully_at(fd, dst.data(), source.size(), 0) ==
                static_cast<ssize_t>(source.size()),
            "read_fully_at did not read the whole file");
      check(std::memcmp(dst.data(), source.data(), source.size()) == 0,
            "read_fully_at returned different bytes");

      check(csd::read_fully_at(fd, dst.data(), source.size(), 2048) == 2048,
            "read_fully_at past EOF must return the short count");

      ::close(fd);
      ::unlink(path);
    }
  }

  // 7. The rounded window can run past EOF: a volume .dat file ends exactly at
  //    the last needle's last byte. Trailing bytes past EOF are never part of
  //    the payload and must not be mistaken for a short read, but a payload that
  //    itself runs past EOF must still fail.
  {
    const int64_t file_size = 4116;  // 20-byte needle head + 4096 payload
    const csd::AlignedWindow window =
        csd::plan_aligned_window(20, 4096, csd::kReadAlignment);
    check(window.read_offset + window.read_size > file_size,
          "expected the rounded window to run past EOF for this layout");

    const int64_t available = csd::available_in_window(window, file_size);
    check(available == file_size,
          "available bytes should stop at EOF, got " + std::to_string(available));
    check(csd::payload_within_available(window, 4096, available),
          "a payload that fits inside the file must not be reported as truncated");
    check(!csd::payload_within_available(window, 4097, available),
          "a payload that runs past EOF must be reported as truncated");

    const csd::AlignedWindow past_eof =
        csd::plan_aligned_window(4096, 4096, csd::kReadAlignment);
    check(csd::available_in_window(past_eof, 1024) == 0,
          "a window entirely past EOF has no available bytes");
  }

  if (failures == 0) {
    std::printf("range_sum_test: all %d checks passed\n", checks);
    return 0;
  }
  std::fprintf(stderr, "range_sum_test: %d of %d checks failed\n", failures, checks);
  return 1;
}
