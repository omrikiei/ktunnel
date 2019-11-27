#!/bin/bash
protoc -I ./tunnel_pb tunnel.proto --go_out=plugins=grpc:tunnel_pb
