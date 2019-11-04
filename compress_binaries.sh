#!/bin/bash
for f in $(find ./dist/ -name 'ktunnel'); do upx $f; done
