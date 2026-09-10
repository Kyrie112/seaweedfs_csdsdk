// csd_agent - persistent SmartSSD compute engine used by SeaweedFS volume.
//
// SeaweedFS sends the exact payload range inside a volume .dat file. This
// agent opens the range, streams it into the FPGA sum kernel (file_sum64) and
// returns only the decimal result. No temporary file is created and no whole
// needle is loaded into the SeaweedFS process.

#define CL_HPP_CL_1_2_DEFAULT_BUILD
#define CL_HPP_TARGET_OPENCL_VERSION 120
#define CL_HPP_MINIMUM_OPENCL_VERSION 120
#define CL_HPP_ENABLE_PROGRAM_CONSTRUCTION_FROM_ARRAY_COMPATIBILITY 1
#define CL_USE_DEPRECATED_OPENCL_1_2_APIS

#include <CL/cl2.hpp>
#include <CL/cl_ext_xilinx.h>

#include "aligned_range.h"

#include <arpa/inet.h>
#include <fcntl.h>
#include <netdb.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <iostream>
#include <memory>
#include <sstream>
#include <string>
#include <vector>

namespace {

#define OCL_CHECK(error, call)                                        \
  call;                                                               \
  if (error != CL_SUCCESS) {                                          \
    throw std::runtime_error(std::string("OpenCL error calling ") + #call); \
  }

// file_sum64 argument counts.
//   v1: (buffer, byte_count, sum_out)
//   v2: (buffer, byte_count, payload_shift, payload_size, sum_out)
// The v2 extra arguments let the device skip the leading bytes of a block
// aligned read window instead of the host shifting the payload, which would
// cost one extra full copy through the P2P mapping.
constexpr int kKernelArgsV1 = 3;
constexpr int kKernelArgsV2 = 5;

// RWF_NOWAIT is defined in <linux/fs.h>, which this file does not include; the
// value is part of the stable uapi.
#ifndef RWF_NOWAIT
#define RWF_NOWAIT 0x00000008
#endif

// Which read path the agent uses for the payload window.
enum class CachePolicy {
  Direct,    // always O_DIRECT
  Buffered,  // always page-cache reads
  Auto,      // probe the page cache, use it on a hit, else O_DIRECT
};

// Reads len bytes into buf at offset only if the range is already resident in
// the page cache. Returns the number of bytes available without blocking, or -1
// with errno set (EAGAIN = not cached, ENOSYS = kernel too old).
ssize_t try_nowait_read(int fd, void* buf, size_t len, int64_t offset) {
#if defined(SYS_preadv2)
  struct iovec iov {};
  iov.iov_base = buf;
  iov.iov_len = len;
  return static_cast<ssize_t>(::syscall(SYS_preadv2, fd, &iov, 1,
                                        static_cast<off_t>(offset), RWF_NOWAIT));
#else
  (void)fd;
  (void)buf;
  (void)len;
  (void)offset;
  errno = ENOSYS;
  return -1;
#endif
}

int64_t elapsed_ms(const std::chrono::steady_clock::time_point& start) {
  return std::chrono::duration_cast<std::chrono::milliseconds>(
             std::chrono::steady_clock::now() - start)
      .count();
}

// Counters that make the aligned path auditable from the agent log. The
// direct_hits/direct_misses pair is the evidence that O_DIRECT is actually
// being used for real SeaweedFS needle payloads.
struct AgentStats {
  uint64_t direct_hits = 0;
  uint64_t direct_misses = 0;
  uint64_t short_reads = 0;
  uint64_t legacy_paths = 0;
  uint64_t window_mismatches = 0;
  // Adaptive-path counters: hits are requests served from the page cache,
  // misses/partials are the ones that fell back to O_DIRECT.
  uint64_t cache_hits = 0;
  uint64_t cache_partial = 0;
  uint64_t cache_misses = 0;
  uint64_t probe_errors = 0;
};

struct ComputeOutcome {
  std::string result;
  int64_t read_offset = 0;
  int64_t read_size = 0;
  int64_t payload_shift = 0;
  int64_t read_bytes = 0;
  int64_t read_ms = 0;
  int64_t probe_ms = 0;
  int64_t kernel_ms = 0;
  bool direct = false;
  bool aligned_kernel = false;
  std::string path;
};

template <typename T>
struct aligned_allocator {
  using value_type = T;
  T* allocate(std::size_t num) {
    void* ptr = nullptr;
    if (posix_memalign(&ptr, 4096, num * sizeof(T))) {
      throw std::bad_alloc();
    }
    return reinterpret_cast<T*>(ptr);
  }
  void deallocate(T* p, std::size_t) { free(p); }
};

std::vector<cl::Device> get_xil_devices() {
  cl_int err = CL_SUCCESS;
  std::vector<cl::Platform> platforms;
  OCL_CHECK(err, err = cl::Platform::get(&platforms));
  for (auto& platform : platforms) {
    std::string name;
    OCL_CHECK(err, name = platform.getInfo<CL_PLATFORM_NAME>(&err));
    if (name != "Xilinx") {
      continue;
    }
    std::vector<cl::Device> devices;
    OCL_CHECK(err, err = platform.getDevices(CL_DEVICE_TYPE_ACCELERATOR, &devices));
    return devices;
  }
  return {};
}

std::string read_binary_file(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw std::runtime_error("cannot open xclbin " + path);
  }
  std::ostringstream ss;
  ss << in.rdbuf();
  return ss.str();
}

struct ComputeRequest {
  std::string operation;
  std::string data_file;
  int64_t offset = 0;
  int64_t size = 0;

  // Aligned read window sent by SeaweedFS (v2 protocol). Negative means the
  // field was absent, in which case the agent recomputes the window itself.
  int64_t read_offset = -1;
  int64_t read_size = -1;
  int64_t payload_shift = -1;
};

std::string json_string(const std::string& body, const std::string& key) {
  const std::string needle = "\"" + key + "\"";
  std::size_t p = body.find(needle);
  if (p == std::string::npos) return "";
  p = body.find(':', p + needle.size());
  if (p == std::string::npos) return "";
  p = body.find('"', p);
  if (p == std::string::npos) return "";
  ++p;
  std::size_t end = body.find('"', p);
  if (end == std::string::npos) return "";
  return body.substr(p, end - p);
}

int64_t json_int(const std::string& body, const std::string& key) {
  const std::string needle = "\"" + key + "\"";
  std::size_t p = body.find(needle);
  if (p == std::string::npos) return 0;
  p = body.find(':', p + needle.size());
  if (p == std::string::npos) return 0;
  return std::strtoll(body.c_str() + p + 1, nullptr, 10);
}

// json_int_or returns fallback when the key is absent, so "0" and "missing"
// stay distinguishable for the optional aligned-window fields.
int64_t json_int_or(const std::string& body, const std::string& key,
                    int64_t fallback) {
  const std::string needle = "\"" + key + "\"";
  if (body.find(needle) == std::string::npos) return fallback;
  return json_int(body, key);
}

ComputeRequest parse_request(const std::string& body) {
  ComputeRequest req;
  req.operation = json_string(body, "operation");
  req.data_file = json_string(body, "data_file");
  req.offset = json_int(body, "offset");
  req.size = json_int(body, "size");
  req.read_offset = json_int_or(body, "read_offset", -1);
  req.read_size = json_int_or(body, "read_size", -1);
  req.payload_shift = json_int_or(body, "payload_shift", -1);
  if (req.operation.empty() || req.data_file.empty() || req.size < 0) {
    throw std::runtime_error("invalid compute request");
  }
  return req;
}

class OpenClRuntime {
 public:
  explicit OpenClRuntime(const std::string& xclbin_path) {
    auto devices = get_xil_devices();
    if (devices.empty()) {
      throw std::runtime_error("no Xilinx accelerator device found");
    }
    device_ = devices[0];
    cl_int err = CL_SUCCESS;
    context_ = std::make_unique<cl::Context>(device_, nullptr, nullptr, nullptr, &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create context failed");
    queue_ = std::make_unique<cl::CommandQueue>(
        *context_, device_, CL_QUEUE_PROFILING_ENABLE, &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create command queue failed");

    const std::string blob = read_binary_file(xclbin_path);
    const cl_device_id dev_id = device_();
    const unsigned char* image = reinterpret_cast<const unsigned char*>(blob.data());
    const size_t image_len = blob.size();
    cl_int binary_status = CL_SUCCESS;
    cl_program raw = ::clCreateProgramWithBinary(
        (*context_)(), 1, &dev_id, &image_len, &image, &binary_status, &err);
    if (err != CL_SUCCESS || raw == nullptr) {
      throw std::runtime_error("create program from binary failed");
    }
    err = ::clBuildProgram(raw, 1, &dev_id, "", nullptr, nullptr);
    if (err != CL_SUCCESS) {
      size_t log_len = 0;
      ::clGetProgramBuildInfo(raw, dev_id, CL_PROGRAM_BUILD_LOG, 0, nullptr, &log_len);
      std::string log(log_len > 0 ? log_len : 1, '\0');
      if (log_len > 0) {
        ::clGetProgramBuildInfo(raw, dev_id, CL_PROGRAM_BUILD_LOG, log_len, &log[0], nullptr);
      }
      throw std::runtime_error("build program failed: " + log);
    }
    program_ = std::make_unique<cl::Program>(raw, false);

    cl::Kernel probe(*program_, "file_sum64", &err);
    if (err != CL_SUCCESS) {
      throw std::runtime_error("kernel file_sum64 not found in xclbin");
    }
    cl_uint arg_count = 0;
    if (probe.getInfo(CL_KERNEL_NUM_ARGS, &arg_count) != CL_SUCCESS) {
      throw std::runtime_error("cannot query file_sum64 argument count");
    }
    if (arg_count != kKernelArgsV1 && arg_count != kKernelArgsV2) {
      throw std::runtime_error(
          "file_sum64 takes " + std::to_string(arg_count) +
          " arguments, expected 3 (v1) or 5 (v2); rebuild agent and xclbin together");
    }
    kernel_arg_count_ = static_cast<int>(arg_count);
  }

  int kernel_arg_count() const { return kernel_arg_count_; }
  const AgentStats& stats() const { return stats_; }
  void set_cache_policy(CachePolicy policy) { cache_policy_ = policy; }
  CachePolicy cache_policy() const { return cache_policy_; }

  // Runs one compute request and reports the device-side cost breakdown.
  ComputeOutcome sum_range(const ComputeRequest& req) {
    if (req.operation != "rawsum64" && req.operation != "file_sum64") {
      throw std::runtime_error("unsupported operation " + req.operation);
    }
    ComputeOutcome outcome;
    if (req.size == 0) {
      outcome.result = "0";
      return outcome;
    }
    if (req.size < 0 || req.offset < 0) {
      throw std::runtime_error("invalid range: negative offset or size");
    }
    outcome.aligned_kernel = (kernel_arg_count_ == kKernelArgsV2);

    // The v2 kernel can skip a leading shift inside an aligned window; the v1
    // kernel cannot, so it must be fed a buffer that starts exactly at the
    // payload - which is exactly why v1 cannot use O_DIRECT for needle data.
    csd::AlignedWindow window =
        csd::plan_aligned_window(req.offset, req.size, csd::kReadAlignment);

    if (req.read_size > 0) {
      const csd::AlignedWindow declared{req.read_offset, req.read_size,
                                        req.payload_shift};
      if (!csd::covers(declared, req.offset, req.size)) {
        // The agent owns its own device constraints: recompute and warn rather
        // than compute a wrong answer over a window that does not cover the
        // payload.
        ++stats_.window_mismatches;
        std::cerr << "csd_agent: ignoring inconsistent aligned window from "
                     "volume server (declared read_offset="
                  << req.read_offset << " read_size=" << req.read_size
                  << " payload_shift=" << req.payload_shift
                  << ", recomputed read_offset=" << window.read_offset
                  << " read_size=" << window.read_size
                  << " payload_shift=" << window.payload_shift << ")\n";
      }
    }
    if (!outcome.aligned_kernel) {
      ++stats_.legacy_paths;
      window = csd::AlignedWindow{req.offset, req.size, 0};
      outcome.path = "legacy";
    }
    outcome.read_offset = window.read_offset;
    outcome.read_size = window.read_size;
    outcome.payload_shift = window.payload_shift;

    cl_int err = CL_SUCCESS;
    const std::size_t byte_count = static_cast<std::size_t>(window.read_size);

    // P2P buffer: map FPGA device memory into the host address space and read
    // the disk range straight into it. No host CL buffer and no XRT migrate of
    // the payload are needed.
    cl_mem_ext_ptr_t ext{};
    ext.flags = XCL_MEM_EXT_P2P_BUFFER;
    cl::Buffer input(*context_, CL_MEM_READ_WRITE | CL_MEM_EXT_PTR_XILINX,
                     byte_count, &ext, &err);
    if (err != CL_SUCCESS) {
      throw std::runtime_error("create P2P input buffer failed");
    }
    unsigned char* p2p = static_cast<unsigned char*>(queue_->enqueueMapBuffer(
        input, CL_TRUE, CL_MAP_WRITE, 0, byte_count, nullptr, nullptr, &err));
    if (p2p == nullptr || err != CL_SUCCESS) {
      throw std::runtime_error("map P2P input buffer failed");
    }
    // Unmap on every exit path, including the error paths below.
    struct Unmapper {
      cl::CommandQueue* queue;
      cl::Buffer* buffer;
      void* ptr;
      bool active;
      ~Unmapper() {
        if (active) queue->enqueueUnmapMemObject(*buffer, ptr);
      }
    } unmapper{queue_.get(), &input, p2p, true};

    int fd = ::open(req.data_file.c_str(), O_RDONLY);
    if (fd < 0) {
      throw std::runtime_error("cannot open data file " + req.data_file);
    }

    // The rounded-up window can run past the end of the volume .dat file, since
    // the file ends exactly at the last needle's last byte. Those trailing bytes
    // are never part of the payload, so read what exists and leave the rest of
    // the buffer untouched. Only a payload that is itself incomplete is an error.
    struct stat file_stat {};
    if (::fstat(fd, &file_stat) != 0) {
      ::close(fd);
      throw std::runtime_error("cannot stat data file " + req.data_file);
    }
    const int64_t available =
        csd::available_in_window(window, static_cast<int64_t>(file_stat.st_size));
    if (!csd::payload_within_available(window, req.size, available)) {
      ::close(fd);
      ++stats_.short_reads;
      throw std::runtime_error(
          "needle payload is truncated: " + req.data_file + " has " +
          std::to_string(available) + " of " +
          std::to_string(window.payload_shift + req.size) +
          " bytes needed at window offset " +
          std::to_string(window.read_offset));
    }

    // O_DIRECT needs an aligned file offset, an aligned length and an aligned
    // buffer address. The window start is aligned by construction; the length
    // must be trimmed to whole blocks, and the sub-block tail is read buffered.
    const int64_t direct_len = available - (available % csd::kReadAlignment);
    const bool window_aligned =
        window.read_offset % csd::kReadAlignment == 0 && direct_len > 0 &&
        reinterpret_cast<uintptr_t>(p2p) % csd::kReadAlignment == 0;
    // Keep a buffered descriptor open as well: an O_DIRECT descriptor rejects
    // the sub-block tail read with EINVAL, so the tail must go through a normal
    // descriptor.
    int fd_direct = -1;
    if (window_aligned) {
      fd_direct = ::open(req.data_file.c_str(), O_RDONLY | O_DIRECT);
      outcome.direct = fd_direct >= 0;
    }
    if (outcome.direct) {
      ++stats_.direct_hits;
    } else {
      ++stats_.direct_misses;
    }

    // Adaptive data path selection. A successful RWF_NOWAIT probe is not a
    // wasted round trip: the bytes land in the destination buffer, so a cache
    // hit costs one memcpy instead of a device read. A miss (EAGAIN) or a
    // partial hit falls back to the O_DIRECT window read below.
    bool served_from_cache = false;
    if (cache_policy_ == CachePolicy::Auto) {
      const auto probe_start = std::chrono::steady_clock::now();
      const ssize_t probed = try_nowait_read(fd, p2p, static_cast<size_t>(available),
                                             window.read_offset);
      const int probe_errno = errno;
      outcome.probe_ms = elapsed_ms(probe_start);
      if (probed == static_cast<ssize_t>(available)) {
        served_from_cache = true;
        ++stats_.cache_hits;
      } else if (probed >= 0) {
        ++stats_.cache_partial;
      } else if (probe_errno == EAGAIN) {
        ++stats_.cache_misses;
      } else {
        ++stats_.probe_errors;
      }
    }

    const auto read_start = std::chrono::steady_clock::now();
    ssize_t got = 0;
    if (served_from_cache) {
      outcome.path = "cache";
      got = static_cast<ssize_t>(available);
    } else if (outcome.direct) {
      got = csd::read_fully_at(fd_direct, p2p, static_cast<size_t>(direct_len),
                               window.read_offset);
      if (got == direct_len && available > direct_len) {
        const ssize_t tail = csd::read_fully_at(
            fd, p2p + direct_len, static_cast<size_t>(available - direct_len),
            window.read_offset + direct_len);
        got = tail < 0 ? tail : got + tail;
      }
    } else {
      got = csd::read_fully_at(fd, p2p, static_cast<size_t>(available),
                               window.read_offset);
    }
    if (outcome.path.empty()) {
      outcome.path = outcome.direct ? "direct" : "buffered";
    }
    outcome.read_ms = elapsed_ms(read_start);
    outcome.read_bytes = got;
    if (fd_direct >= 0) {
      ::close(fd_direct);
    }
    ::close(fd);
    if (got != static_cast<ssize_t>(available)) {
      ++stats_.short_reads;
      throw std::runtime_error("short read of " + req.data_file + ": wanted " +
                               std::to_string(available) + " bytes at offset " +
                               std::to_string(window.read_offset) + ", got " +
                               std::to_string(got));
    }
    OCL_CHECK(err, err = queue_->enqueueUnmapMemObject(input, p2p));
    unmapper.active = false;

    std::vector<uint64_t, aligned_allocator<uint64_t>> result(1, 0);
    cl::Buffer output(*context_, CL_MEM_WRITE_ONLY | CL_MEM_USE_HOST_PTR,
                      sizeof(uint64_t), result.data(), &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create output buffer failed");

    cl::Kernel kernel(*program_, "file_sum64", &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create kernel failed");
    unsigned arg = 0;
    OCL_CHECK(err, err = kernel.setArg(arg++, input));
    OCL_CHECK(err, err = kernel.setArg(arg++, static_cast<uint32_t>(byte_count)));
    if (outcome.aligned_kernel) {
      OCL_CHECK(err, err = kernel.setArg(
                          arg++, static_cast<uint32_t>(window.payload_shift)));
      OCL_CHECK(err, err = kernel.setArg(
                          arg++, static_cast<uint32_t>(req.size)));
    }
    OCL_CHECK(err, err = kernel.setArg(arg++, output));

    const auto kernel_start = std::chrono::steady_clock::now();
    OCL_CHECK(err, err = queue_->enqueueTask(kernel));
    OCL_CHECK(err, err = queue_->enqueueMigrateMemObjects({output},
                                                         CL_MIGRATE_MEM_OBJECT_HOST));
    OCL_CHECK(err, err = queue_->finish());
    outcome.kernel_ms = elapsed_ms(kernel_start);
    outcome.result = std::to_string(result[0]);
    return outcome;
  }

 private:
  cl::Device device_;
  std::unique_ptr<cl::Context> context_;
  std::unique_ptr<cl::CommandQueue> queue_;
  std::unique_ptr<cl::Program> program_;
  int kernel_arg_count_ = 0;
  CachePolicy cache_policy_ = CachePolicy::Auto;
  AgentStats stats_;
};

bool send_all(int fd, const std::string& data) {
  std::size_t sent = 0;
  while (sent < data.size()) {
    const ssize_t n = ::send(fd, data.data() + sent, data.size() - sent, MSG_NOSIGNAL);
    if (n <= 0) return false;
    sent += static_cast<std::size_t>(n);
  }
  return true;
}

void respond(int fd, int status, const std::string& body) {
  std::ostringstream head;
  head << "HTTP/1.1 " << status << " " << (status == 200 ? "OK" : "Error")
       << "\r\nContent-Type: application/json\r\nContent-Length: "
       << body.size() << "\r\nConnection: close\r\n\r\n";
  send_all(fd, head.str() + body);
}

int create_listener(const std::string& endpoint) {
  const std::size_t colon = endpoint.rfind(':');
  const std::string host = endpoint.substr(0, colon);
  const int port = std::stoi(endpoint.substr(colon + 1));

  addrinfo hints{};
  hints.ai_family = AF_UNSPEC;
  hints.ai_socktype = SOCK_STREAM;
  hints.ai_flags = AI_PASSIVE;
  addrinfo* result = nullptr;
  const std::string port_str = std::to_string(port);
  if (getaddrinfo(host.empty() ? nullptr : host.c_str(), port_str.c_str(),
                  &hints, &result) != 0) {
    throw std::runtime_error("cannot resolve listen address " + endpoint);
  }
  int listen_fd = -1;
  for (addrinfo* ai = result; ai; ai = ai->ai_next) {
    listen_fd = ::socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
    if (listen_fd < 0) continue;
    int one = 1;
    ::setsockopt(listen_fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    if (::bind(listen_fd, ai->ai_addr, ai->ai_addrlen) == 0) break;
    ::close(listen_fd);
    listen_fd = -1;
  }
  freeaddrinfo(result);
  if (listen_fd < 0 || ::listen(listen_fd, 16) != 0) {
    throw std::runtime_error("cannot listen on " + endpoint);
  }
  return listen_fd;
}

void handle_client(int client_fd, OpenClRuntime& runtime) {
  std::string raw;
  char buf[4096];
  for (;;) {
    const ssize_t n = ::recv(client_fd, buf, sizeof(buf), 0);
    if (n <= 0) break;
    raw.append(buf, static_cast<std::size_t>(n));
    const std::size_t header_end = raw.find("\r\n\r\n");
    if (header_end == std::string::npos) continue;

    std::size_t body_start = header_end + 4;
    std::size_t content_length = 0;
    const std::size_t cl = raw.find("Content-Length:");
    if (cl != std::string::npos) {
      content_length = static_cast<std::size_t>(
          std::strtoull(raw.c_str() + cl + 15, nullptr, 10));
    }
    while (raw.size() < body_start + content_length) {
      const ssize_t m = ::recv(client_fd, buf, sizeof(buf), 0);
      if (m <= 0) break;
      raw.append(buf, static_cast<std::size_t>(m));
    }
    const std::string body = raw.substr(body_start, content_length);
    if (raw.rfind("POST ", 0) == 0 && raw.find("/v1/compute") != std::string::npos) {
      try {
        const ComputeRequest req = parse_request(body);
        const ComputeOutcome outcome = runtime.sum_range(req);
        std::ostringstream response;
        response << "{\"result\":\"" << outcome.result << "\""
                 << ",\"read_ms\":" << outcome.read_ms
                 << ",\"kernel_ms\":" << outcome.kernel_ms
                 << ",\"direct\":" << (outcome.direct ? "true" : "false")
                 << ",\"path\":\"" << outcome.path << "\""
                 << ",\"probe_ms\":" << outcome.probe_ms
                 << ",\"aligned_kernel\":"
                 << (outcome.aligned_kernel ? "true" : "false") << "}";
        const AgentStats& stats = runtime.stats();
        std::cerr << "csd_agent: size=" << req.size
                  << " window=[" << outcome.read_offset << ","
                  << (outcome.read_offset + outcome.read_size) << ")"
                  << " shift=" << outcome.payload_shift
                  << " kernel=" << (outcome.aligned_kernel ? "v2" : "v1")
                  << " path=" << outcome.path
                  << " direct=" << (outcome.direct ? "yes" : "no")
                  << " probe_ms=" << outcome.probe_ms
                  << " read_bytes=" << outcome.read_bytes
                  << " read_ms=" << outcome.read_ms
                  << " kernel_ms=" << outcome.kernel_ms
                  << " direct_hits=" << stats.direct_hits
                  << " direct_misses=" << stats.direct_misses
                  << " short_reads=" << stats.short_reads
                  << " cache_hits=" << stats.cache_hits
                  << " cache_misses=" << stats.cache_misses
                  << " cache_partial=" << stats.cache_partial << "\n";
        respond(client_fd, 200, response.str());
      } catch (const std::exception& e) {
        respond(client_fd, 500, "{\"error\":\"" + std::string(e.what()) + "\"}");
      }
    } else {
      respond(client_fd, 404, "{\"error\":\"not found\"}");
    }
    break;
  }
  ::close(client_fd);
}

}  // namespace

int main(int argc, char** argv) {
  std::string xclbin;
  std::string listen = "127.0.0.1:18090";
  std::string cache_policy = "auto";
  for (int i = 1; i < argc; ++i) {
    const std::string arg = argv[i];
    if (arg.rfind("--xclbin=", 0) == 0) {
      xclbin = arg.substr(9);
    } else if (arg.rfind("--listen=", 0) == 0) {
      listen = arg.substr(9);
    } else if (arg.rfind("--cache-policy=", 0) == 0) {
      cache_policy = arg.substr(15);
    } else {
      std::cerr << "usage: csd_agent --xclbin=<file> [--listen=ip:port]"
                   " [--cache-policy=auto|direct|buffered]\n";
      return 2;
    }
  }
  if (xclbin.empty()) {
    std::cerr << "missing --xclbin\n";
    return 2;
  }

  try {
    OpenClRuntime runtime(xclbin);
    if (cache_policy == "direct") {
      runtime.set_cache_policy(CachePolicy::Direct);
    } else if (cache_policy == "buffered") {
      runtime.set_cache_policy(CachePolicy::Buffered);
    } else if (cache_policy == "auto") {
      runtime.set_cache_policy(CachePolicy::Auto);
    } else {
      std::cerr << "unknown --cache-policy=" << cache_policy
                << " (want auto, direct or buffered)\n";
      return 2;
    }
    std::cerr << "csd_agent: cache policy=" << cache_policy << "\n";
    std::cerr << "csd_agent: file_sum64 args=" << runtime.kernel_arg_count()
              << " ("
              << (runtime.kernel_arg_count() == kKernelArgsV2
                      ? "v2: aligned window supported, O_DIRECT used for needle payloads"
                      : "v1: legacy payload-aligned reads only, no O_DIRECT for needles")
              << ")\n";
    const int listen_fd = create_listener(listen);
    std::cerr << "csd_agent ready on " << listen << "\n";
    for (;;) {
      const int client_fd = ::accept(listen_fd, nullptr, nullptr);
      if (client_fd < 0) continue;
      handle_client(client_fd, runtime);
    }
  } catch (const std::exception& e) {
    std::cerr << "csd_agent fatal: " << e.what() << "\n";
    return 1;
  }
  return 0;
}
