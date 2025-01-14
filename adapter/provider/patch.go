package provider

import "time"

var suspended bool

type UpdatableProvider interface {
	UpdatedAt() time.Time
}

func (f *proxySetProvider) UpdatedAt() time.Time {
	return f.Fetcher.UpdatedAt
}

func (pp *proxySetProvider) Close() error {
	pp.healthCheck.close()
	pp.Fetcher.Destroy()

	return nil
}

func (cp *compatibleProvider) Close() error {
	cp.healthCheck.close()

	return nil
}

func Suspend(s bool) {
	suspended = s
}
