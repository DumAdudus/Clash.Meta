package resource

import (
	"context"
	"io"
	"net/http"
	"os"
	"time"

	netHttp "github.com/Dreamacro/clash/component/http"
	types "github.com/Dreamacro/clash/constant/provider"
)

type FileVehicle struct {
	path string
}

func (f *FileVehicle) Type() types.VehicleType {
	return types.File
}

func (f *FileVehicle) Path() string {
	return f.path
}

func (f *FileVehicle) Read() ([]byte, error) {
	return os.ReadFile(f.path)
}

func NewFileVehicle(path string) *FileVehicle {
	return &FileVehicle{path: path}
}

type HTTPVehicle struct {
	url  string
	path string
}

func (h *HTTPVehicle) Type() types.VehicleType {
	return types.HTTP
}

func (h *HTTPVehicle) Path() string {
	return h.path
}

func (h *HTTPVehicle) Read() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*20)
	defer cancel()
	resp, err := netHttp.HttpRequest(ctx, h.url, http.MethodGet, nil, nil)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func NewHTTPVehicle(url string, path string) *HTTPVehicle {
	return &HTTPVehicle{url, path}
}
