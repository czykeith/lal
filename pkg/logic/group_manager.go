// Copyright 2021, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import "sync"

// ---------------------------------------------------------------------------------------------------------------------

type IGroupCreator interface {
	CreateGroup(appName, streamName string) *Group
}

type ModConfigGroupCreator func(appName, streamName string, baseConfig *Config)

// IGroupManager
//
// 封装管理Group的容器
// 管理流标识（appName，streamName）与Group的映射关系。比如appName是否参与映射匹配
type IGroupManager interface {
	// GetOrCreateGroup
	//
	// @param appName     注意，如果没有，可以为""
	// @return createFlag 如果为false表示group之前就存在，如果为true表示为当前新创建
	//
	GetOrCreateGroup(appName string, streamName string) (group *Group, createFlag bool)

	// GetGroup
	//
	// @return 如果不存在，则返回nil
	//
	GetGroup(appName string, streamName string) *Group

	// Iterate 遍历所有 Group
	//
	// @param onIterateGroup 如果返回false，则删除这个 Group
	//
	Iterate(onIterateGroup func(group *Group) bool)

	Len() int

	// TODO(chef): feat 没有提供删除操作，因为目前使用时，是在遍历时做删除的
}

// ---------------------------------------------------------------------------------------------------------------------

// SimpleGroupManager 忽略appName，只使用streamName
type SimpleGroupManager struct {
	groupCreator IGroupCreator
	groups       map[string]*Group // streamName -> Group
	mutex        sync.RWMutex      // 保护 groups 的并发访问
}

func NewSimpleGroupManager(groupCreator IGroupCreator) *SimpleGroupManager {
	return &SimpleGroupManager{
		groupCreator: groupCreator,
		groups:       make(map[string]*Group),
	}
}

func (s *SimpleGroupManager) GetOrCreateGroup(appName string, streamName string) (group *Group, createFlag bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	g, ok := s.groups[streamName]
	if !ok {
		g = s.groupCreator.CreateGroup(appName, streamName)
		if g != nil {
			s.groups[streamName] = g
			return g, true
		}
		// 如果创建失败，返回 nil
		return nil, false
	}
	return g, false
}

func (s *SimpleGroupManager) GetGroup(appName string, streamName string) *Group {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	g, ok := s.groups[streamName]
	if !ok {
		return nil
	}
	return g
}

func (s *SimpleGroupManager) Iterate(onIterateGroup func(group *Group) bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// 收集需要删除的 key
	var toDelete []string
	for streamName, group := range s.groups {
		if group == nil {
			// 如果 group 为 nil，也应该删除
			toDelete = append(toDelete, streamName)
			continue
		}
		if !onIterateGroup(group) {
			toDelete = append(toDelete, streamName)
		}
	}

	// 删除标记的 group
	for _, streamName := range toDelete {
		delete(s.groups, streamName)
	}
}

func (s *SimpleGroupManager) Len() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return len(s.groups)
}

// ---------------------------------------------------------------------------------------------------------------------

// ComplexGroupManager
//
// 注意，这个模块的功能不完全，目前只使用SimpleGroupManager
//
// TODO(chef):
//
// - 现有逻辑重构至当前模块中【DONE】
// - server_manger接入当前模块，替换掉原来的map【DONE】
// - 重构整理使用server_manager的地方【DONE】
// - 实现appName逻辑的IGroupManager【DONE】
// - 增加单元测试【DONE】
// - 配置文件或var.go中增加选取具体IGroupManager实现的开关
// - 去除配置文件中一部分的url_pattern
// - 更新相应的文档：本文件注释，server_manager等中原有关于appName的注释，配置文件文档，流地址列表文档
// - 创建group时没有appname，后面又有了，可以考虑更新一下
// - ComplexGroupManager使用IGroupCreator
//
// ---------------------------------------------------------------------------------------------------------------------
//
// 背景：
//   - 有的协议需要结合appName和streamName作为流唯一标识（比如rtmp，httpflv，httpts）。
//   - 有的协议不需要appName，只使用streamName作为流唯一标识（比如rtsp？）。
//
// 目标：
//   - 有appName的协议，需要参考appName。
//   - 没appName的协议，需要和有appName的协议互通。
//
// 注意：
//   - 当以上两种类型的协议混用时，系统使用者应避免第二种协议的streamName，在第一种协议中存在相同的streamName，但是appName不止一个，这种情况下，内部无法知道该如何对应。
//   - group可能由第一种协议创建，也可能由第二种协议创建。
type ComplexGroupManager struct {
	groupCreator IGroupCreator
	// 注意，一个group只可能在一个容器中，两个容器中的group加起来才是全量
	onlyStreamNameGroups    map[string]*Group            // streamName -> Group
	appNameStreamNameGroups map[string]map[string]*Group // appName -> streamName -> Group
	mutex                   sync.RWMutex                 // 保护所有 maps 的并发访问
}

func NewComplexGroupManager(groupCreator IGroupCreator) *ComplexGroupManager {
	return &ComplexGroupManager{
		groupCreator:            groupCreator,
		onlyStreamNameGroups:    make(map[string]*Group),
		appNameStreamNameGroups: make(map[string]map[string]*Group),
	}
}

func (gm *ComplexGroupManager) GetOrCreateGroup(appName string, streamName string) (group *Group, createFlag bool) {
	return gm.getGroup(appName, streamName, true)
}

func (gm *ComplexGroupManager) GetGroup(appName string, streamName string) *Group {
	g, _ := gm.getGroup(appName, streamName, false)
	return g
}

func (gm *ComplexGroupManager) getGroup(appName string, streamName string, shouldCreate bool) (group *Group, createFlag bool) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	var ok bool
	if appName == "" {
		group, ok = gm.onlyStreamNameGroups[streamName]
		if ok {
			return group, false
		}
		// 虽然没有appName，也有可能在appNameStreamNameGroups中，我们遍历查找
		//
		// 注意，此时有可能不同appName的容器里都有对应这个streamName的group，但是程序已没法区分，系统使用者应规范流名称避免出现这种问题
		//
		for _, m := range gm.appNameStreamNameGroups {
			group, ok = m[streamName]
			if ok {
				return group, false
			}
		}

		// 两个容器都没找到
		if shouldCreate {
			group = gm.groupCreator.CreateGroup(appName, streamName)
			if group != nil {
				gm.onlyStreamNameGroups[streamName] = group
				return group, true
			}
			// 如果创建失败，返回 nil
			return nil, false
		} else {
			return nil, false
		}
	} else { // appName存在
		// 先在对应appName中查找
		m, mok := gm.appNameStreamNameGroups[appName]
		if mok {
			group, ok = m[streamName]
			if ok {
				return group, false
			}
		}

		// 虽然有appName，也有可能在onlyStreamNameGroups中，我们尝试一下
		group, ok = gm.onlyStreamNameGroups[streamName]
		if ok {
			return group, false
		}

		// 都没有找到
		if shouldCreate {
			group = gm.groupCreator.CreateGroup(appName, streamName)
			if group != nil {
				if !mok {
					m = make(map[string]*Group)
					gm.appNameStreamNameGroups[appName] = m
				}
				m[streamName] = group
				return group, true
			}
			// 如果创建失败，返回 nil
			return nil, false
		} else {
			return nil, false
		}
	}
}

func (gm *ComplexGroupManager) Iterate(onIterateGroup func(group *Group) bool) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	// 收集需要删除的 key
	var toDeleteOnlyStreamName []string
	for streamName, group := range gm.onlyStreamNameGroups {
		if group == nil {
			// 如果 group 为 nil，也应该删除
			toDeleteOnlyStreamName = append(toDeleteOnlyStreamName, streamName)
			continue
		}
		if !onIterateGroup(group) {
			toDeleteOnlyStreamName = append(toDeleteOnlyStreamName, streamName)
		}
	}

	// 删除标记的 group
	for _, streamName := range toDeleteOnlyStreamName {
		delete(gm.onlyStreamNameGroups, streamName)
	}

	// 收集需要删除的 appName 和 streamName
	var toDeleteAppNameStreamName []struct {
		appName    string
		streamName string
	}
	var toDeleteAppName []string

	for appName, m := range gm.appNameStreamNameGroups {
		for streamName, group := range m {
			if group == nil {
				// 如果 group 为 nil，也应该删除
				toDeleteAppNameStreamName = append(toDeleteAppNameStreamName, struct {
					appName    string
					streamName string
				}{appName: appName, streamName: streamName})
				continue
			}
			if !onIterateGroup(group) {
				toDeleteAppNameStreamName = append(toDeleteAppNameStreamName, struct {
					appName    string
					streamName string
				}{appName: appName, streamName: streamName})
			}
		}
	}

	// 删除标记的 group
	for _, item := range toDeleteAppNameStreamName {
		if m, ok := gm.appNameStreamNameGroups[item.appName]; ok {
			delete(m, item.streamName)
			if len(m) == 0 {
				toDeleteAppName = append(toDeleteAppName, item.appName)
			}
		}
	}

	// 删除空的 appName
	for _, appName := range toDeleteAppName {
		delete(gm.appNameStreamNameGroups, appName)
	}
}

func (gm *ComplexGroupManager) Len() int {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	var c int
	for _, m := range gm.appNameStreamNameGroups {
		c += len(m)
	}
	return c + len(gm.onlyStreamNameGroups)
}
