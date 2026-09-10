// Compile-check stub for the OpenCL C++ bindings. See README.md.
//
// It mirrors only the API surface src/csd_agent.cpp uses, so the agent can be
// type-checked without XRT. Calls return success and empty results; nothing
// here is ever executed.
#ifndef CSD_STUB_CL2_HPP
#define CSD_STUB_CL2_HPP

#include <cstddef>
#include <string>
#include <vector>

typedef int cl_int;
typedef unsigned int cl_uint;
typedef unsigned long cl_bitfield;
typedef unsigned long cl_ulong;
typedef struct _cl_mem* cl_mem;
typedef struct _cl_program* cl_program;
typedef struct _cl_device_id* cl_device_id;
typedef struct _cl_context* cl_context;
typedef struct _cl_command_queue* cl_command_queue;
typedef struct _cl_kernel* cl_kernel;

#define CL_SUCCESS 0
#define CL_PLATFORM_NAME 0x0900
#define CL_DEVICE_TYPE_ACCELERATOR (1 << 5)
#define CL_MEM_READ_WRITE (1 << 0)
#define CL_MEM_WRITE_ONLY (1 << 1)
#define CL_MEM_USE_HOST_PTR (1 << 3)
#define CL_MEM_EXT_PTR_XILINX (1 << 5)
#define CL_MAP_WRITE (1 << 1)
#define CL_TRUE 1
#define CL_QUEUE_PROFILING_ENABLE (1 << 1)
#define CL_KERNEL_NUM_ARGS 0x11B6
#define CL_MIGRATE_MEM_OBJECT_HOST (1 << 0)
#define CL_PROGRAM_BUILD_LOG 0x1183

#include "cl_ext_xilinx.h"

namespace cl {

class Device {
 public:
  cl_device_id operator()() const { return nullptr; }
};

class Platform {
 public:
  static cl_int get(std::vector<Platform>* platforms) {
    if (platforms != nullptr) {
      platforms->clear();
    }
    return CL_SUCCESS;
  }

  template <int Name>
  std::string getInfo(cl_int* err) const {
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
    return std::string();
  }

  cl_int getDevices(cl_bitfield type, std::vector<Device>* devices) const {
    (void)type;
    if (devices != nullptr) {
      devices->clear();
    }
    return CL_SUCCESS;
  }
};

class Buffer;
class Kernel;
class Program;

class Context {
 public:
  Context(const Device&, void*, void*, void*, cl_int* err) {
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
  }
  cl_context operator()() const { return nullptr; }
};

class Program {
 public:
  Program(cl_program raw, bool take_ownership) {
    (void)raw;
    (void)take_ownership;
  }
};

class Kernel {
 public:
  Kernel(const Program&, const char* name, cl_int* err) {
    (void)name;
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
  }

  cl_int getInfo(cl_uint param, cl_uint* value) const {
    (void)param;
    if (value != nullptr) {
      *value = 3;
    }
    return CL_SUCCESS;
  }

  template <typename T>
  cl_int setArg(cl_uint index, const T& value) {
    (void)index;
    (void)value;
    return CL_SUCCESS;
  }
};

class Buffer {
 public:
  Buffer(const Context&, cl_bitfield flags, std::size_t size, cl_int* err) {
    (void)flags;
    (void)size;
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
  }

  Buffer(const Context&, cl_bitfield flags, std::size_t size,
         cl_mem_ext_ptr_t* ext, cl_int* err) {
    (void)flags;
    (void)size;
    (void)ext;
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
  }

  Buffer(const Context&, cl_bitfield flags, std::size_t size, void* host_ptr,
         cl_int* err) {
    (void)flags;
    (void)size;
    (void)host_ptr;
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
  }
};

class CommandQueue {
 public:
  CommandQueue(const Context&, const Device&, cl_bitfield props, cl_int* err) {
    (void)props;
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
  }

  void* enqueueMapBuffer(const Buffer& buffer, cl_uint blocking,
                         cl_bitfield flags, std::size_t offset,
                         std::size_t size, void* events, void** event,
                         cl_int* err) {
    (void)buffer;
    (void)blocking;
    (void)flags;
    (void)offset;
    (void)size;
    (void)events;
    (void)event;
    if (err != nullptr) {
      *err = CL_SUCCESS;
    }
    return nullptr;
  }

  cl_int enqueueUnmapMemObject(const Buffer&, void*) { return CL_SUCCESS; }

  cl_int enqueueTask(const Kernel&) { return CL_SUCCESS; }

  cl_int enqueueMigrateMemObjects(const std::vector<Buffer>&, cl_bitfield) {
    return CL_SUCCESS;
  }

  cl_int finish() { return CL_SUCCESS; }
};

}  // namespace cl

inline cl_program clCreateProgramWithBinary(cl_context, cl_uint, const cl_device_id*,
                                            const std::size_t*,
                                            const unsigned char**,
                                            cl_int* binary_status, cl_int* err) {
  if (binary_status != nullptr) {
    *binary_status = CL_SUCCESS;
  }
  if (err != nullptr) {
    *err = CL_SUCCESS;
  }
  return nullptr;
}

inline cl_int clBuildProgram(cl_program, cl_uint, const cl_device_id*, const char*,
                             void*, void*) {
  return CL_SUCCESS;
}

inline cl_int clGetProgramBuildInfo(cl_program, cl_device_id, cl_uint, std::size_t,
                                    void*, std::size_t*) {
  return CL_SUCCESS;
}

#endif  // CSD_STUB_CL2_HPP
