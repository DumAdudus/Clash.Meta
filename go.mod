module github.com/Dreamacro/clash

go 1.19

require (
	github.com/HyNetwork/hysteria v1.2.2
	github.com/dlclark/regexp2 v1.7.0
	github.com/go-chi/chi/v5 v5.0.7
	github.com/go-chi/cors v1.2.1
	github.com/go-chi/render v1.0.2
	github.com/gofrs/uuid v4.2.0+incompatible
	github.com/gorilla/websocket v1.5.0
	github.com/insomniacslk/dhcp v0.0.0-20220822114210-de18a9d48e84
	github.com/lucas-clemente/quic-go v0.30.0
	github.com/lunixbochs/struc v0.0.0-20200707160740-784aaebc1d40
	github.com/miekg/dns v1.1.50
	github.com/oschwald/geoip2-golang v1.8.0
	github.com/sagernet/sing v0.0.0-20220801112236-1bb95f9661fc
	github.com/sagernet/sing-shadowsocks v0.0.0-20220801112336-a91eacdd01e1
	github.com/sagernet/sing-vmess v0.0.0-20220801112355-e1de36a3c90e
	github.com/sirupsen/logrus v1.9.0
	github.com/stretchr/testify v1.8.0
	github.com/xtls/go v0.0.0-20210920065950-d4af136d3672
	go.etcd.io/bbolt v1.3.6
	go.uber.org/atomic v1.10.0
	go.uber.org/automaxprocs v1.5.1
	golang.org/x/crypto v0.0.0-20220824171710-5757bc0c5503
	golang.org/x/exp v0.0.0-20220823124025-807a23277127
	golang.org/x/net v0.0.0-20220822230855-b0a4917ee28c
	golang.org/x/sync v0.0.0-20220819030929-7fc1605a5dde
	golang.org/x/sys v0.0.0-20220908164124-27713097b956
	google.golang.org/protobuf v1.28.1
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/lucas-clemente/quic-go => github.com/HyNetwork/quic-go v0.30.1-0.20221023055600-93b146ab9c48

// replace github.com/HyNetwork/hysteria => github.com/DumAdudus/hysteria v0.0.0-20220902030938-f22705be2c71

require (
	github.com/ajg/form v1.5.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/go-task/slim-sprig v0.0.0-20210107165309-348f09dbbbc0 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/google/pprof v0.0.0-20210407192527-94a9f03dee38 // indirect
	github.com/klauspost/cpuid/v2 v2.1.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/marten-seemann/qpack v0.3.0 // indirect
	github.com/marten-seemann/qtls-go1-18 v0.1.3 // indirect
	github.com/marten-seemann/qtls-go1-19 v0.1.1 // indirect
	github.com/onsi/ginkgo/v2 v2.2.0 // indirect
	github.com/oschwald/maxminddb-golang v1.10.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/u-root/uio v0.0.0-20220204230159-dac05f7d2cb4 // indirect
	golang.org/x/mod v0.6.0-dev.0.20220419223038-86c51ed26bb4 // indirect
	golang.org/x/text v0.3.8-0.20220124021120-d1c84af989ab // indirect
	golang.org/x/tools v0.1.12 // indirect
	lukechampine.com/blake3 v1.1.7 // indirect
)
