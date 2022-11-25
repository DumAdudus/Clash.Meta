package geodata

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Dreamacro/clash/common/convert"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"
)

var (
	initFlag        bool
	geoUpdatePeriod = 1 // day
)

func InitGeoSite() error {
	geoFilePath := C.Path.GeoSite()
	geoFileInfo, err := os.Stat(geoFilePath)
	if os.IsNotExist(err) {
		log.Infoln("Can't find GeoSite.dat, start download")
		if err := downloadGeoSite(C.Path.GeoSite()); err != nil {
			return fmt.Errorf("can't download GeoSite.dat: %s", err.Error())
		}
		log.Infoln("Download GeoSite.dat finish")
	}
	if !initFlag {
		needUpdate := false
		if geoFileInfo != nil {
			needUpdate = geoFileInfo.ModTime().AddDate(0, 0, geoUpdatePeriod).Before(time.Now())
		}
		err := Verify(C.GeositeName)
		if needUpdate || err != nil {
			if needUpdate {
				log.Infoln("GeoSite.dat needs update")
			} else {
				log.Warnln("GeoSite.dat invalid, remove and download: %s", err)
			}
			geoFilePathTmp := geoFilePath + ".tmp"
			if err := downloadGeoSite(geoFilePathTmp); err != nil {
				log.Errorln("can't download GeoSite.dat: %s", err.Error())
			} else {
				if err := os.Rename(geoFilePathTmp, geoFilePath); err != nil {
					return fmt.Errorf("can't replace GeoSite.dat: %s", err.Error())
				}
			}
		}
		initFlag = true
	}
	return nil
}

func downloadGeoSite(path string) (err error) {
	// resp, err := getUrl("https://ghproxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/geosite.dat")
	log.Infoln("Download %s", C.GeoSiteUrl)
	resp, err := getUrl(C.GeoSiteUrl)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var respBody io.Reader
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		respBody, err = gzip.NewReader(resp.Body)
	} else {
		respBody = resp.Body
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, respBody)

	return err
}

func getUrl(url string) (resp *http.Response, err error) {
	var req *http.Request
	req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	convert.SetUserAgent(req.Header)
	req.Header.Add("Accept-Encoding", "gzip")
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr == "ghproxy.com:443" {
					addr = "146.56.146.190:443" // kr1.ops.ci
				}
				return (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext(ctx, network, addr)
			},
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	resp, err = httpClient.Do(req)
	return
}
