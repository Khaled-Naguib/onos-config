// Copyright 2019-present Open Networking Foundation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"fmt"
	"github.com/onosproject/onos-config/pkg/utils"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/goyang/pkg/yang"
	"github.com/openconfig/ygot/ygot"
	log "k8s.io/klog"
	"plugin"
	"regexp"
	"strings"
)

// ModelPlugin is a set of methods that each model plugin should implement
type ModelPlugin interface {
	ModelData() (string, string, []*gnmi.ModelData, string)
	UnmarshalConfigValues(jsonTree []byte) (*ygot.ValidatedGoStruct, error)
	Validate(*ygot.ValidatedGoStruct, ...ygot.ValidationOption) error
	Schema() (map[string]*yang.Entry, error)
}

// RegisterModelPlugin adds an external model plugin to the model registry at startup
// or through the 'admin' gRPC interface. Once plugins are loaded they cannot be unloaded
func (m *Manager) RegisterModelPlugin(moduleName string) (string, string, error) {
	log.Info("Loading module ", moduleName)
	modelPluginModule, err := plugin.Open(moduleName)
	if err != nil {
		log.Warning("Unable to load module ", moduleName)
		return "", "", err
	}
	symbolMP, err := modelPluginModule.Lookup("ModelPlugin")
	if err != nil {
		log.Warning("Unable to find ModelPlugin in module ", moduleName)
		return "", "", err
	}
	modelPlugin, ok := symbolMP.(ModelPlugin)
	if !ok {
		log.Warning("Unable to use ModelPlugin in ", moduleName)
		return "", "", fmt.Errorf("symbol loaded from module %s is not a ModelPlugin",
			moduleName)
	}
	name, version, _, _ := modelPlugin.ModelData()
	modelName := utils.ToModelName(name, version)
	m.ModelRegistry[modelName] = modelPlugin
	modelschema, err := modelPlugin.Schema()
	if err != nil {
		log.Warning("Error loading schema from model plugin", modelName, err)
		return "", "", err
	}

	m.ModelReadOnlyPaths[modelName] = extractReadOnlyPaths(modelschema["Device"],
		yang.TSUnset, "", "")
	log.Infof("Model %s %s loaded. %d read only paths", name, version,
		len(m.ModelReadOnlyPaths[modelName]))
	return name, version, nil
}

// Capabilities returns an aggregated set of modelData in gNMI capabilities format
// with duplicates removed
func (m *Manager) Capabilities() []*gnmi.ModelData {
	// Make a map - if we get duplicates overwrite them
	modelMap := make(map[string]*gnmi.ModelData)
	for _, model := range m.ModelRegistry {
		_, _, modelItem, _ := model.ModelData()
		for _, mi := range modelItem {
			modelName := utils.ToModelName(mi.Name, mi.Version)
			modelMap[modelName] = mi
		}
	}

	outputList := make([]*gnmi.ModelData, len(modelMap))
	i := 0
	for _, modelItem := range modelMap {
		outputList[i] = modelItem
		i++
	}
	return outputList
}

// extractReadOnlyPaths is a recursive function to extract a list of read only paths from a YGOT schema
func extractReadOnlyPaths(deviceEntry *yang.Entry, parentState yang.TriState, parentNs string, parentPath string) []string {
	readOnlyPaths := make([]string, 0)

	for _, dirEntry := range deviceEntry.Dir {
		namespace := extractnamespace(dirEntry, parentNs)
		itemPath := formatName(dirEntry, false, parentNs, parentPath)
		if dirEntry.IsLeaf() {
			// No need to recurse
			if dirEntry.Config == yang.TSFalse || parentState == yang.TSFalse {
				readOnlyPaths = append(readOnlyPaths, itemPath)
			}
		} else if dirEntry.IsContainer() {
			if dirEntry.Config == yang.TSFalse || parentState == yang.TSFalse {
				readOnlyPaths = append(readOnlyPaths, itemPath)
				continue // No need to add child paths is this is "config false"
			}
			newPaths := extractReadOnlyPaths(dirEntry, dirEntry.Config, namespace, itemPath)
			for _, newPath := range newPaths {
				readOnlyPaths = append(readOnlyPaths, newPath)
			}
		} else if dirEntry.IsList() {
			itemPath = formatName(dirEntry, true, parentNs, parentPath)
			if dirEntry.Config == yang.TSFalse || parentState == yang.TSFalse {
				readOnlyPaths = append(readOnlyPaths, itemPath)
				continue // No need to add child paths is this is "config false"
			}
			newPaths := extractReadOnlyPaths(dirEntry, dirEntry.Config, namespace, itemPath)
			for _, newPath := range newPaths {
				readOnlyPaths = append(readOnlyPaths, newPath)
			}
		}
	}

	return readOnlyPaths
}

// RemovePathIndices removes the index value from a path to allow it to be compared to a model path
func RemovePathIndices(path string) string {
	const indexPattern = `=.*?]`
	rname := regexp.MustCompile(indexPattern)
	indices := rname.FindAllStringSubmatch(path, -1)
	for _, i := range indices {
		path = strings.Replace(path, i[0], "=*]", 1)
	}
	return path
}

func formatName(dirEntry *yang.Entry, isList bool, parentNs string, parentPath string) string {
	namespace := extractnamespace(dirEntry, parentNs)

	var name string
	if namespace == parentNs && isList {
		name = fmt.Sprintf("%s/%s[%s=*]", parentPath, dirEntry.Name, dirEntry.Key)
	} else if isList {
		name = fmt.Sprintf("%s/%s:%s[%s=*]", parentPath, namespace, dirEntry.Name, dirEntry.Key)
	} else if namespace == parentNs || namespace == "" {
		name = fmt.Sprintf("%s/%s", parentPath, dirEntry.Name)
	} else {
		name = fmt.Sprintf("%s/%s:%s", parentPath, namespace, dirEntry.Name)
	}

	return name
}

func extractnamespace(dirEntry *yang.Entry, parentNs string) string {
	namespace := dirEntry.Namespace()
	if namespace != nil && namespace.Name != "" {
		return namespace.Name
	}

	prefix := dirEntry.Prefix.Name
	// Special case until YGOT gets fixed - doesn't return namespaces
	if prefix == "openflow" {
		return "openconfig-openflow"
	} else if prefix == "oc-log" {
		return "openconfig-system-logging"
	} else if prefix == "oc-proc" {
		return "openconfig-procmon"
	} else if prefix == "oc-sys-term" {
		return "openconfig-system-terminal"
	} else if prefix == "oc-aaa" {
		return "openconfig-aaa"
	}

	if dirEntry.Annotation != nil {
		schemaPath, nsok := dirEntry.Annotation["schemapath"]
		if nsok {
			nsstr, ok := schemaPath.(string)
			if ok {
				nselem := strings.Split(nsstr, "/")
				if len(nselem) > 1 {
					return nselem[1]
				}
			}
		}
	}
	return parentNs
}
