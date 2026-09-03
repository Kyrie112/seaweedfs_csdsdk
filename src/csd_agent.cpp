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

#include <arpa/inet.h>
#include <fcntl.h>
#include <netdb.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include <algorithm>
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

ComputeRequest parse_request(const std::string& body) {
  ComputeRequest req;
  req.operation = json_string(body, "operation");
  req.data_file = json_string(body, "data_file");
  req.offset = json_int(body, "offset");
  req.size = json_int(body, "size");
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
  }

  // Returns decimal result string.
  std::string sum_range(const ComputeRequest& req) {
    if (req.operation != "rawsum64" && req.operation != "file_sum64") {
      throw std::runtime_error("unsupported operation " + req.operation);
    }
    const std::size_t word_count = static_cast<std::size_t>((req.size + 7) / 8);
    if (word_count == 0) {
      return "0";
    }
    std::vector<uint64_t, aligned_allocator<uint64_t>> words(word_count, 0);

    const int fd = ::open(req.data_file.c_str(), O_RDONLY);
    if (fd < 0) {
      throw std::runtime_error("cannot open data file " + req.data_file);
    }
    const ssize_t got = ::pread(fd, words.data(), static_cast<std::size_t>(req.size),
                                static_cast<off_t>(req.offset));
    ::close(fd);
    if (got < 0) {
      throw std::runtime_error("cannot read data range " + req.data_file);
    }
    // words were pre-zeroed; partial tail is treated as zero-padded.

    cl_int err = CL_SUCCESS;
    const std::size_t input_bytes = word_count * sizeof(uint64_t);
    cl::Buffer input(*context_, CL_MEM_READ_ONLY | CL_MEM_USE_HOST_PTR,
                     input_bytes, words.data(), &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create input buffer failed");
    uint64_t result = 0;
    cl::Buffer output(*context_, CL_MEM_WRITE_ONLY | CL_MEM_USE_HOST_PTR,
                      sizeof(uint64_t), &result, &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create output buffer failed");

    cl::Kernel kernel(*program_, "file_sum64", &err);
    if (err != CL_SUCCESS) throw std::runtime_error("create kernel failed");
    unsigned arg = 0;
    OCL_CHECK(err, err = kernel.setArg(arg++, input));
    OCL_CHECK(err, err = kernel.setArg(arg++, static_cast<uint32_t>(word_count)));
    OCL_CHECK(err, err = kernel.setArg(arg++, output));
    OCL_CHECK(err, err = queue_->enqueueMigrateMemObjects({input}, 0));
    OCL_CHECK(err, err = queue_->enqueueTask(kernel));
    OCL_CHECK(err, err = queue_->enqueueMigrateMemObjects({output},
                                                         CL_MIGRATE_MEM_OBJECT_HOST));
    OCL_CHECK(err, err = queue_->finish());
    return std::to_string(result);
  }

 private:
  cl::Device device_;
  std::unique_ptr<cl::Context> context_;
  std::unique_ptr<cl::CommandQueue> queue_;
  std::unique_ptr<cl::Program> program_;
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
        const std::string result = runtime.sum_range(req);
        respond(client_fd, 200, "{\"result\":\"" + result + "\"}");
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
  for (int i = 1; i < argc; ++i) {
    const std::string arg = argv[i];
    if (arg.rfind("--xclbin=", 0) == 0) {
      xclbin = arg.substr(9);
    } else if (arg.rfind("--listen=", 0) == 0) {
      listen = arg.substr(9);
    } else {
      std::cerr << "usage: csd_agent --xclbin=<file> [--listen=ip:port]\n";
      return 2;
    }
  }
  if (xclbin.empty()) {
    std::cerr << "missing --xclbin\n";
    return 2;
  }

  try {
    OpenClRuntime runtime(xclbin);
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
