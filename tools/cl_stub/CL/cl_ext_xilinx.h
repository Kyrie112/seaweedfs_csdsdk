// Compile-check stub for the Xilinx OpenCL extension header. See README.md.
#ifndef CSD_STUB_CL_EXT_XILINX_H
#define CSD_STUB_CL_EXT_XILINX_H

#define XCL_MEM_EXT_P2P_BUFFER (1 << 0)

typedef struct {
  unsigned flags;
  void* obj;
  void* param;
} cl_mem_ext_ptr_t;

#endif  // CSD_STUB_CL_EXT_XILINX_H
