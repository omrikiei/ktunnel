GOBIN := $(CURDIR)/bin
export GOBIN

export PATH := $(GOBIN):$(PATH)

GO_TOOLS := \
	github.com/akuity/grpc-gateway-client/protoc-gen-grpc-gateway-client \
	github.com/bufbuild/buf/cmd/buf \
	github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway \
	github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2 \
	google.golang.org/grpc/cmd/protoc-gen-go-grpc \
	google.golang.org/protobuf/cmd/protoc-gen-go


.PHONY: tools
tools:
	@go install $(GO_TOOLS)

build:
	@go build

docs: build
	@GEN_DOC=true ./ktunnel version

.PHONY: proto
proto: tools
	@PATH=$(GOBIN):$(PATH) buf generate

.PHONY: clean
clean:
	@rm -rf $(GOBIN)