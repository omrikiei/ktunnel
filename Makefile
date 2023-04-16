build:
	@go build

docs: build
	@GEN_DOC=true ./ktunnel version
