#!/usr/bin/env bash

package_name=ktunnel
platforms=("windows/amd64" "windows/386" "darwin/amd64" "linux/386" "linux/amd64")

for platform in "${platforms[@]}"
do
    platform_split=(${platform//\// })
    GOOS=${platform_split[0]}
    GOARCH=${platform_split[1]}
    output_name=$package_name
    if [ $GOOS = "windows" ]; then
        output_name+='.exe'
    fi

    OUT_EXEC=./releases/$GOOS-$GOARCH/$output_name
    mkdir -p ./releases/$GOOS-$GOARCH
    echo "Building for $GOOS/$GOARCH"
    env GOOS=$GOOS GOARCH=$GOARCH go build -o $OUT_EXEC -ldflags="-s -w"
    if [ $? -ne 0 ]; then
        echo 'An error has occurred! Aborting the script execution...'
        exit 1
    fi
    upx $OUT_EXEC
    TAR_OUT=$(echo $OUT_EXEC | sed 's/\.exe//g')
    tar cvzf ${TAR_OUT}_${GOOS}_${GOARCH}.tar.gz $OUT_EXEC
done
