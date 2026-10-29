#!/usr/bin/env bash
# Generate the Go and Python stubs from env/proto/livesched_env.proto.
# Needs protoc, protoc-gen-go and protoc-gen-go-grpc on PATH (Go), and
# `pip install grpcio grpcio-tools` (Python).
set -euo pipefail
cd "$(dirname "$0")/../.."
mkdir -p env/envpb
protoc -I env/proto --go_out=env/envpb --go_opt=paths=source_relative \
  --go-grpc_out=env/envpb --go-grpc_opt=paths=source_relative env/proto/livesched_env.proto
python3 -m grpc_tools.protoc -I env/proto --python_out=env/python --grpc_python_out=env/python env/proto/livesched_env.proto
