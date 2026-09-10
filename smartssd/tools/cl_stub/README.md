# OpenCL stub headers (compile check only)

These headers are **not** a real OpenCL implementation and are never linked into
the agent binary. They exist so `src/csd_agent.cpp` can be syntax- and
type-checked on a workstation that has no XRT/OpenCL installation:

```bash
g++ -fsyntax-only -std=c++17 -Wall -Wextra -I tools/cl_stub src/csd_agent.cpp
```

The real build still needs the Xilinx runtime (`make`, requires
`/opt/xilinx/xrt`). A full build and hardware run happens on the SmartSSD host.

If the agent starts using more of the OpenCL C++ API, extend these stubs
accordingly - they are only required to cover the calls the agent makes.
