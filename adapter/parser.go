package adapter

import (
	"fmt"

	"github.com/Dreamacro/clash/adapter/outbound"
	"github.com/Dreamacro/clash/common/structure"
	C "github.com/Dreamacro/clash/constant"
)

func ParseProxy(mapping map[string]any) (C.Proxy, error) {
	decoder := structure.NewDecoder(structure.Option{TagName: "proxy", WeaklyTypedInput: true})
	proxyType, existType := mapping["type"].(string)
	if !existType {
		return nil, fmt.Errorf("missing type")
	}

	var (
		proxy C.ProxyAdapter
		err   error
	)
	switch proxyType {
	case "ss":
		ssOption := &outbound.ShadowSocksOption{}
		err = decoder.Decode(mapping, ssOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewShadowSocks(*ssOption)
	case "socks5":
		socksOption := &outbound.Socks5Option{}
		err = decoder.Decode(mapping, socksOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewSocks5(*socksOption)
	case "http":
		httpOption := &outbound.HttpOption{}
		err = decoder.Decode(mapping, httpOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewHttp(*httpOption)
	case "vmess":
		vmessOption := &outbound.VmessOption{
			HTTPOpts: outbound.HTTPOptions{
				Method: "GET",
				Path:   []string{"/"},
			},
		}
		err = decoder.Decode(mapping, vmessOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewVmess(*vmessOption)
	case "vless":
		vlessOption := &outbound.VlessOption{}
		err = decoder.Decode(mapping, vlessOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewVless(*vlessOption)
	case "trojan":
		trojanOption := &outbound.TrojanOption{}
		err = decoder.Decode(mapping, trojanOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewTrojan(*trojanOption)
	case "hysteria":
		hyOption := &outbound.HysteriaOption{}
		err = decoder.Decode(mapping, hyOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewHysteria(*hyOption)
	case "tuic":
		tuicOption := &outbound.TuicOption{}
		err = decoder.Decode(mapping, tuicOption)
		if err != nil {
			break
		}
		proxy, err = outbound.NewTuic(*tuicOption)
	default:
		return nil, fmt.Errorf("unsupport proxy type: %s", proxyType)
	}

	if err != nil {
		return nil, err
	}

	return NewProxy(proxy), nil
}
