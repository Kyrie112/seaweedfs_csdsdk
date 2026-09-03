CC ?= g++
CXXFLAGS ?= -O2 -std=c++17 -Wall -Wextra
XRT ?= /opt/xilinx/xrt
OPENCL_INC := -I$(XRT)/include -I$(XRT)/include/CL
OPENCL_LIB := -L$(XRT)/lib -lOpenCL -Wl,-rpath,$(XRT)/lib

all: build/csd_agent

build:
	mkdir -p build

build/csd_agent: src/csd_agent.cpp | build
	$(CC) $(CXXFLAGS) $(OPENCL_INC) -o $@ src/csd_agent.cpp $(OPENCL_LIB)

clean:
	rm -rf build

.PHONY: all clean
