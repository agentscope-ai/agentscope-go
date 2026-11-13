module github.com/agentscope-ai/agentscope-go/v2

go 1.25.0

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/gin-gonic/gin v1.12.0
	github.com/google/uuid v1.6.0
	github.com/labstack/echo/v4 v4.15.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/open-dingtalk/dingtalk-stream-sdk-go v0.9.1
	github.com/prometheus/client_golang v1.24.1
	github.com/qdrant/go-client v1.19.0
	github.com/sirupsen/logrus v1.9.4
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
	golang.org/x/sys v0.47.0
	google.golang.org/grpc v1.83.0
	gopkg.in/yaml.v3 v3.0.1
	mvdan.cc/sh/v3 v3.13.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic v1.15.0 // indirect
	github.com/bytedance/sonic/loader v0.5.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/gabriel-vasile/mimetype v1.4.12 // indirect
	github.com/gin-contrib/sse v1.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.1 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/labstack/gommon v0.5.0 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.59.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.3.1 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.mongodb.org/mongo-driver/v2 v2.5.0 // indirect
	golang.org/x/arch v0.22.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260807164820-c8921c73eeea // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// v2.1.0 (2026-07-06) and v2.1.1 (2026-08-10) were tagged and later deleted from
// the repository, but the module proxy serves deleted versions indefinitely.
// Neither was a real release, yet both outrank the v2.0.x line, so an
// unqualified `@latest` query resolves to v2.1.1 (the proxy's own /@latest
// endpoint reports v2.0.10; the go command takes the version-list maximum
// instead). Retractions are read from the latest version's go.mod, so these take
// effect only in a release higher than v2.1.1. Forensics are in the commit that
// added them.
retract (
	v2.1.1 // second tag on the commit released as v2.0.6; deleted from the repository
	v2.1.0 // 2026-07-06 commit, older than v2.0.4; deleted from the repository
)
