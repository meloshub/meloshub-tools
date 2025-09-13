package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log"
	"os"

	"github.com/meloshub/meloshub/adapter"
	"gopkg.in/yaml.v3"
)

type UpdateEntry struct {
	Before adapter.Metadata `json:"before"`
	After  adapter.Metadata `json:"after"`
}
type ChangeReport struct {
	Added   []adapter.Metadata `json:"added"`
	Removed []adapter.Metadata `json:"removed"`
	Updated []UpdateEntry      `json:"updated"`
}

func main() {
	oldFile := flag.String("old", "", "Path to the old metadata YAML file")
	newFile := flag.String("new", "", "Path to the new metadata YAML file")
	outputFile := flag.String("output", "changes.json", "Path to the output JSON report file")
	flag.Parse()

	if *oldFile == "" || *newFile == "" {
		log.Fatal("Both --old and --new file paths are required.")
	}

	var oldMetadata []adapter.Metadata
	oldData, err := os.ReadFile(*oldFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("Old metadata file '%s' not found. Assuming all new adapters are 'Added'.", *oldFile)
			oldMetadata = []adapter.Metadata{} // 将旧元数据视为空列表
		} else {
			// 如果是其他错误，则终止
			log.Fatalf("Error reading old metadata file: %v", err)
		}
	} else {
		// 如果文件存在，正常解析
		if err := yaml.Unmarshal(oldData, &oldMetadata); err != nil {
			log.Fatalf("Could not parse old yaml file %s: %v", *oldFile, err)
		}
	}

	// 读取和解析新文件
	newData, err := os.ReadFile(*newFile)
	if err != nil {
		log.Fatalf("Error reading new metadata file: %v", err)
	}
	var newMetadata []adapter.Metadata
	if err := yaml.Unmarshal(newData, &newMetadata); err != nil {
		log.Fatalf("Could not parse new yaml file %s: %v", *newFile, err)
	}

	// 比较并生成报告
	report := compareMetadata(oldMetadata, newMetadata)

	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatalf("Error marshalling report to JSON: %v", err)
	}
	if err := os.WriteFile(*outputFile, reportJSON, 0644); err != nil {
		log.Fatalf("Error writing output report file: %v", err)
	}
	log.Printf("Successfully generated change report to %s", *outputFile)
}

// compareMetadata 比较元数据变动
func compareMetadata(oldList, newList []adapter.Metadata) ChangeReport {
	oldMap := make(map[string]adapter.Metadata)
	for _, m := range oldList {
		oldMap[m.Id] = m
	}

	newMap := make(map[string]adapter.Metadata)
	for _, m := range newList {
		newMap[m.Id] = m
	}

	report := ChangeReport{}

	// 适配器新增与更新检查
	for id, newMeta := range newMap {
		oldMeta, exists := oldMap[id]
		if !exists {
			// 如果旧文件中不存在此ID，视为新增的适配器
			report.Added = append(report.Added, newMeta)
		} else {
			oldYAML, _ := yaml.Marshal(oldMeta)
			newYAML, _ := yaml.Marshal(newMeta)
			if string(oldYAML) != string(newYAML) {
				report.Updated = append(report.Updated, UpdateEntry{Before: oldMeta, After: newMeta})
			}
		}
	}

	// 适配器移除检查
	for id, oldMeta := range oldMap {
		if _, exists := newMap[id]; !exists {
			report.Removed = append(report.Removed, oldMeta)
		}
	}

	return report
}
