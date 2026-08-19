module github.com/SakuraOpenSource/levis-epay-plugin

go 1.26.4

require (
	github.com/SakuraOpenSource/levis v0.0.0
	google.golang.org/grpc v1.83.0
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// 本地依赖主项目
replace github.com/SakuraOpenSource/levis => ../levis
