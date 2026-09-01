build:
	@go build

docs: build
	@GEN_DOC=true ./ktunnel version
proto:
	@buf generate

# Integration tests need a real kubelet: they assert that the pod starts, that
# it can read the credentials ktunnel generated for it, and that traffic
# actually crosses the tunnel. A fake client can see none of that, which is how
# v2.4.0 shipped a crash loop past a green suite.
KIND_CLUSTER ?= ktunnel-itest
ITEST_IMAGE  ?= ktunnel:itest

test-integration: build
	docker build -t $(ITEST_IMAGE) .
	kind create cluster --name $(KIND_CLUSTER) 2>/dev/null || true
	kind load docker-image $(ITEST_IMAGE) --name $(KIND_CLUSTER)
	KTUNNEL_BIN=$(PWD)/ktunnel \
	KTUNNEL_IMAGE=$(ITEST_IMAGE) \
	KTUNNEL_TEST_CONTEXT=kind-$(KIND_CLUSTER) \
		go test -tags integration -v -timeout 20m -count=1 ./test/integration/...

test-integration-clean:
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: test-integration test-integration-clean
