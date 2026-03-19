// Copyright 2022, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import "github.com/q191201771/lal/pkg/hls"

func (group *Group) IsHlsMuxerAlive() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.hlsMuxer != nil
}

// startHlsIfNeeded 必要时启动hls
func (group *Group) startHlsIfNeeded() {
	if !group.config.HlsConfig.Enable && !group.config.HlsConfig.EnableHttps {
		return
	}

	// memory-as-disk 模式下，如果频繁 stop/start 拉流导致 muxer 重建，
	// 旧的 ts/m3u8 文件可能不在新 muxer 的环形队列视野内，难以及时被 asap 策略淘汰，
	// 进而造成内存文件系统中历史文件累积。
	// 在创建新 muxer 前先清空该 streamName 的输出目录，保证占用有上界。
	if group.hlsMuxer == nil && group.config.HlsConfig.UseMemoryAsDiskFlag {
		outPath := hls.PathStrategy.GetMuxerOutPath(group.config.HlsConfig.OutPath, group.streamName)
		_ = hls.RemoveAll(outPath)
	}

	group.hlsMuxer = hls.NewMuxer(group.streamName, &group.config.HlsConfig.MuxerConfig, group)
	group.hlsMuxer.Start()
}

func (group *Group) stopHlsIfNeeded() {
	if !group.config.HlsConfig.Enable && !group.config.HlsConfig.EnableHttps {
		return
	}

	if group.hlsMuxer != nil {
		group.hlsMuxer.Dispose()
		group.observer.CleanupHlsIfNeeded(group.appName, group.streamName, group.hlsMuxer.OutPath())
		group.hlsMuxer = nil
	}
}
