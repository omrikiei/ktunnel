build:
	@go build

docs: build
	@GEN_DOC=true ./ktunnel version
proto:
	@buf generate
