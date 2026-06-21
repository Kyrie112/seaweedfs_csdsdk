# SeaweedFS CSD Usage and Test Report

This document records how to use the current compute-storage integration and the
runtime results from the three-node test deployment.

## 1. Request Format

Users send a normal file read request through the Filer and append the compute
operation name in the `compute` query parameter.

```bash
curl "http://<filer_ip>:<filer_port>/<file_path>?compute=<operation_name>"
```

Example:

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=sum"
```

The request means:

```text
file path      = /dataset/big_numbers.txt
operation name = sum
```

The execution path is:

```text
curl client
  -> Filer resolves the file path to the SeaweedFS fileId
  -> Filer proxies the request to the owning Volume server
  -> Volume parses compute=sum
  -> Volume reads the target needle data
  -> Volume writes the needle data to a temporary input file
  -> Volume executes the matching shell script
  -> script output is returned as the HTTP response body
```

## 2. Script Lookup Rule

The Volume server is started with:

```bash
-volume.compute.enabled=true
-volume.compute.dir=/home/ubuntu/csd-test/scripts
-volume.compute.timeout=20s
-volume.compute.maxOutputMB=16
```

For a request:

```text
?compute=sum
```

the Volume server looks for one of the following scripts:

```text
/home/ubuntu/csd-test/scripts/sum
/home/ubuntu/csd-test/scripts/sum.sh
```

The operation name is validated before lookup. Unsafe names such as `../sum`
are rejected.

## 3. Script Input Contract

Before executing the script, the Volume server materializes the needle data as a
temporary file.

The script receives the input in three ways:

```text
argv[1]                         temporary input file path
stdin / fd 0                    opened input file handle
SEAWEED_COMPUTE_INPUT_FILE      temporary input file path
SEAWEED_COMPUTE_INPUT_FD        0
SEAWEED_COMPUTE_OP              operation name
SEAWEED_COMPUTE_FILE_NAME       original file name
SEAWEED_COMPUTE_FILE_ID         SeaweedFS fileId
SEAWEED_COMPUTE_MIME            content type, if available
```

A script can read the data either from stdin:

```bash
awk '{s += $1} END {print s}' <&0
```

or from the temporary file path:

```bash
awk '{s += $1} END {print s}' "$1"
```

## 4. Normal Request Examples

Assume the Filer is available at `10.206.0.4:8888`.

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=sum"
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=linecount"
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=bytes"
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=meta"
```

The same path can be used with a public Filer address if the cloud security
group exposes the Filer port:

```bash
curl "http://119.45.246.156:8888/dataset/big_numbers.txt?compute=sum"
```

If public access to `8888` is not open, use the internal address from another
machine in the same private network:

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=sum"
```

## 5. Test File

`big_numbers.txt` is a test file uploaded to SeaweedFS under:

```text
/dataset/big_numbers.txt
```

It is a normal user file. The CSD path does not depend on this specific file
name. Any uploaded file can be used with the same request format:

```bash
curl "http://<filer_ip>:8888/<your/path/file.txt>?compute=<operation_name>"
```

## 6. Test Deployment

The three test machines were deployed under:

```text
/home/ubuntu/csd-test
```

Cluster layout:

| Public IP | Internal IP | Role |
| --- | --- | --- |
| `119.45.246.156` | `10.206.0.4` | Master, Filer, Volume |
| `118.195.135.56` | `10.206.0.3` | Volume |
| `129.211.163.229` | `10.206.0.11` | Volume |

Running processes:

```text
10.206.0.4:
  weed master -port=9333
  weed filer  -port=8888
  weed volume -port=8080 -volume.compute.enabled=true

10.206.0.3:
  weed volume -port=8080 -volume.compute.enabled=true

10.206.0.11:
  weed volume -port=8080 -volume.compute.enabled=true
```

Installed scripts on all Volume nodes:

```text
/home/ubuntu/csd-test/scripts/bytes.sh
/home/ubuntu/csd-test/scripts/linecount.sh
/home/ubuntu/csd-test/scripts/meta.sh
/home/ubuntu/csd-test/scripts/sum.sh
```

## 7. Runtime Test Results

Tests were run from the servers using the internal Filer address
`10.206.0.4:8888`.

### 7.1 Filer-level Compute Requests

```bash
curl "http://10.206.0.4:8888/dataset/numbers.txt?compute=sum"
```

Result:

```text
15
HTTP 200
```

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=sum"
```

Result:

```text
45000
HTTP 200
```

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=linecount"
```

Result:

```text
10000
HTTP 200
```

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=bytes"
```

Result:

```text
20000
HTTP 200
```

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=meta"
```

Result:

```text
op=meta
file=big_numbers.txt
input=/tmp/seaweed-compute-needle-...
fd=0
HTTP 200
```

The `meta` result proves that the script received both:

```text
temporary input file path = /tmp/seaweed-compute-needle-...
stdin file descriptor     = 0
```

### 7.2 Direct Volume-level Compute Request

The tested file was stored on volume `1`, with file URL:

```text
/1/025af73855/big_numbers.txt
```

Direct Volume request:

```bash
curl "http://127.0.0.1:8080/1/025af73855/big_numbers.txt?compute=sum"
```

Result:

```text
45000
HTTP 200
```

Direct Volume metadata request:

```bash
curl "http://127.0.0.1:8080/1/025af73855/big_numbers.txt?compute=meta"
```

Result:

```text
op=meta
file=big_numbers.txt
input=/tmp/seaweed-compute-needle-...
fd=0
HTTP 200
```

This confirms the operation is executed at the Volume layer, not only at the
Filer layer.

### 7.3 Invalid Operation Test

```bash
curl "http://10.206.0.4:8888/dataset/big_numbers.txt?compute=../sum"
```

Result:

```json
{"error":"invalid compute operation \"../sum\""}
```

Status:

```text
HTTP 400
```

The Volume log also recorded:

```text
response method:GET URL:/1/025af73855/big_numbers.txt?compute=..%2Fsum with httpStatus:400
```

## 8. Notes From Logs

The Master log shows all three Volume servers joined the cluster:

```text
added volume server 10.206.0.4:8080
added volume server 10.206.0.3:8080
added volume server 10.206.0.11:8080
```

The Volume logs show each Volume server started with the expected data
directory and registered its local volumes.

Successful compute operations currently return script output to the HTTP client
but do not emit a dedicated success log line. Failed operations, such as invalid
operation names, are recorded through the normal HTTP error logging path.

## 9. Conclusion

The tested CSD path works for normal shell scripts:

```text
sum.sh       OK
linecount.sh OK
bytes.sh     OK
meta.sh      OK
```

The request format is:

```text
http://<filer_ip>:<filer_port>/<file_path>?compute=<operation_name>
```

The Volume layer correctly parses the operation name, reads the needle data,
passes it to the script as a temporary file and stdin file handle, executes the
script, and returns the script output as the HTTP response.
