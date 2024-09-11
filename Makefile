build:
	@go build

install:
	@go install

docs: build
	@GEN_DOC=true ./ktunnel version
