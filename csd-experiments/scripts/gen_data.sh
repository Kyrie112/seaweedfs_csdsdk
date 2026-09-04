#!/usr/bin/env bash
# Regenerate the 176MB experiment dataset (36,000,000 integers, deterministic).
# Usage: ./gen_data.sh [lines]
LINES=${1:-36000000}
perl -e 'srand(42); for($i=0;$i<$ARGV[0];$i++){ printf "%d\n", int(rand()*10000) }' "$LINES" > big_numbers.txt
echo "wrote $LINES samples to big_numbers.txt (expected sum 179986461084)"
