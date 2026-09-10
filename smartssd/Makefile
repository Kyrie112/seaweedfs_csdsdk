CXX := g++
CXXFLAGS ?= -O2 -std=c++17 -Wall -Wextra
XRT ?= /opt/xilinx/xrt
OPENCL_INC := -I$(XRT)/include -I/usr/include -I/usr/include/CL
OPENCL_LIB := -L$(XRT)/lib -l:libxilinxopencl.so -Wl,-rpath,$(XRT)/lib

all: build/csd_agent

build:
	mkdir -p build

build/csd_agent: src/csd_agent.cpp src/aligned_range.h | build
	$(CXX) $(CXXFLAGS) $(OPENCL_INC) -o $@ src/csd_agent.cpp $(OPENCL_LIB)

# Host-side reference test: needs neither XRT nor an FPGA. It proves that
# reading the enclosing 512B-aligned window and skipping payload_shift bytes
# gives bit-identical results to a payload-aligned read, which is the property
# the v2 kernel relies on.
build/range_sum_test: src/range_sum_test.cpp src/aligned_range.h src/image_topk.h | build
	$(CXX) $(CXXFLAGS) -o $@ src/range_sum_test.cpp

test: build/range_sum_test
	./build/range_sum_test

# Type-check the agent on a workstation without XRT installed, using the
# compile-only stubs in tools/cl_stub.
syntax-check:
	$(CXX) -fsyntax-only $(CXXFLAGS) -I tools/cl_stub src/csd_agent.cpp

clean:
	rm -rf build

.PHONY: all clean test syntax-check
