all: prom2mqtt_amd64 prom2mqtt_arm7 prom2mqtt_arm64

test:
	go test ./...

prom2mqtt_amd64: main.go
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o prom2mqtt_amd64 -ldflags '-s -w' ./

prom2mqtt_arm7: main.go
	env CC=arm-none-eabi-gcc CGO_ENABLED=0  GOOS=linux GOARCH=arm GOARM=7 go build -buildmode=exe -o prom2mqtt_arm7 -ldflags '-extldflags "-fno-PIC static" -s -w' -tags 'osusergo netgo static_build' ./

prom2mqtt_arm64: main.go
	env CGO_ENABLED=0  GOOS=linux GOARCH=arm64 go build -buildmode=exe -o prom2mqtt_arm64 -ldflags '-extldflags "-fno-PIC static" -s -w' -tags 'osusergo netgo static_build' ./


