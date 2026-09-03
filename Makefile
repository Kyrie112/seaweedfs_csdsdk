CXX := g++
CXXFLAGS ?= -O2 -std=c++17 -Wall -Wextra
XRT ?= /opt/xilinx/xrt
OPENCL_INC := -I$(XRT)/include -I/usr/include -I/usr/include/CL
OPENCL_LIB := -L$(XRT)/lib -l:libxilinxopencl.so -Wl,-rpath,$(XRT)/lib

all: build/csd_agent

build:
	mkdir -p build

build/csd_agent: src/csd_agent.cpp | build
	$(CXX) $(CXXFLAGS) $(OPENCL_INC) -o $@ src/csd_agent.cpp $(OPENCL_LIB)

clean:
	rm -rf build

.PHONY: all clean
