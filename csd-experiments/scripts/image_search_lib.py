"""Shared helpers for the near-storage image similarity search experiment.

The descriptor and distance below are the *reference* implementation of the
device operator (see csd-agent-sim/main.go and smartssd_compute_module's
src/image_descriptor.h). The experiment compares the device answer against this
implementation to prove functional equivalence.
"""

import base64
import math
import struct

IMAGE_SIDE = 64
RECORD_SIZE = IMAGE_SIDE * IMAGE_SIDE  # 4096 bytes, 64x64 grayscale
DESCRIPTOR_DIMS = 68  # 64 histogram bins + 4 quadrant sums


def descriptor(record):
    """Integer-only descriptor: 64 histogram bins + 4 quadrant sums."""
    d = [0] * DESCRIPTOR_DIMS
    half = IMAGE_SIDE // 2
    for y in range(IMAGE_SIDE):
        base = y * IMAGE_SIDE
        for x in range(IMAGE_SIDE):
            v = record[base + x]
            d[v >> 2] += 1
            q = (1 if x >= half else 0) + (2 if y >= half else 0)
            d[64 + q] += v
    return d


def distance(a, b):
    return sum(abs(x - y) for x, y in zip(a, b))


def encode_query(query_record):
    d = descriptor(query_record)
    return base64.b64encode(struct.pack("<%di" % DESCRIPTOR_DIMS, *d)).decode()


def brute_force(payload, query_descriptor, k):
    """Reference top-k over the whole gallery, returns [(index, distance), ...]."""
    hits = []
    for index in range(len(payload) // RECORD_SIZE):
        record = payload[index * RECORD_SIZE:(index + 1) * RECORD_SIZE]
        hits.append((index, distance(query_descriptor, descriptor(record))))
    hits.sort(key=lambda hit: (hit[1], hit[0]))
    return hits[:k]


def parse_hits(text):
    text = text.strip()
    if not text:
        return []
    out = []
    for entry in text.split(","):
        index, dist = entry.split(":")
        out.append((int(index), int(dist)))
    return out


def make_gallery(path, images, clusters=8, seed=1234):
    """Synthesise a gallery of visually clustered images.

    Each cluster has a distinct smooth prototype; every image is its prototype
    plus deterministic noise, so images of the same cluster really are similar
    and a query drawn from one cluster has a well defined ground truth.
    """
    state = seed & 0xFFFFFFFF

    def rand():
        nonlocal state
        state = (state * 1103515245 + 12345) & 0xFFFFFFFF
        return state

    prototypes = []
    for c in range(clusters):
        base = 32 + (c * 191) % 160
        period = 4 + c % 5
        proto = bytearray(RECORD_SIZE)
        for y in range(IMAGE_SIDE):
            for x in range(IMAGE_SIDE):
                ripple = (x // period + y // period) % 3
                value = base + ripple * 24 + (x * (c + 1) // IMAGE_SIDE)
                proto[y * IMAGE_SIDE + x] = max(0, min(255, value))
        prototypes.append(proto)

    labels = []
    with open(path, "wb") as handle:
        for i in range(images):
            cluster = i % clusters
            proto = prototypes[cluster]
            record = bytearray(RECORD_SIZE)
            for j in range(RECORD_SIZE):
                noise = (rand() % 17) - 8
                record[j] = max(0, min(255, proto[j] + noise))
            handle.write(record)
            labels.append(cluster)
    return labels


def make_query(proto_index, gallery_path, seed=99):
    """Build a query image from one gallery record plus independent noise."""
    with open(gallery_path, "rb") as handle:
        handle.seek(proto_index * RECORD_SIZE)
        record = bytearray(handle.read(RECORD_SIZE))
    state = seed & 0xFFFFFFFF
    for j in range(RECORD_SIZE):
        state = (state * 1103515245 + 12345) & 0xFFFFFFFF
        record[j] = max(0, min(255, record[j] + (state % 25) - 12))
    return bytes(record)
