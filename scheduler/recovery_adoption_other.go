//go:build !unix

package scheduler

import "errors"

// lockRecoveryAdoption 在非 Unix 平台暂不降级实现：宁可不能自动接管，
// 也不能在没有互斥保证的情况下偷锁造成双控。
func lockRecoveryAdoption(string) (func() error, error) {
	return nil, errors.New("active worker recovery is unsupported on this platform")
}
