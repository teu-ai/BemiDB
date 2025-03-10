package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/linkedin/goavro"
	"github.com/xitongsys/parquet-go/parquet"
	"github.com/xitongsys/parquet-go/reader"
	"github.com/xitongsys/parquet-go/schema"
	"github.com/xitongsys/parquet-go/source"
	"github.com/xitongsys/parquet-go/writer"
)

const (
	PARQUET_PARALLEL_NUMBER  = 4
	PARQUET_ROW_GROUP_SIZE   = 128 * 1024 * 1024 // 128 MB
	PARQUET_COMPRESSION_TYPE = parquet.CompressionCodec_ZSTD

	VERSION_HINT_FILE_NAME      = "version-hint.text"
	INTERNAL_METADATA_FILE_NAME = "bemidb.json"
)

type MetadataJson struct {
	Schemas []struct {
		Fields []struct {
			ID       int         `json:"id"`
			Name     string      `json:"name"`
			Type     interface{} `json:"type"`
			Required bool        `json:"required"`
		} `json:"fields"`
	} `json:"schemas"`
}

type StorageUtils struct {
	config *Config
}

func (storage *StorageUtils) ParseIcebergTableFields(metadataContent []byte) ([]IcebergTableField, error) {
	var metadataJson MetadataJson
	err := json.Unmarshal(metadataContent, &metadataJson)
	if err != nil {
		return nil, err
	}

	var icebergTableFields []IcebergTableField
	for _, schema := range metadataJson.Schemas {
		if schema.Fields != nil {
			for _, field := range schema.Fields {
				icebergTableField := IcebergTableField{
					Name: field.Name,
				}

				if reflect.TypeOf(field.Type).Kind() == reflect.String {
					icebergTableField.Type = field.Type.(string)
					icebergTableField.Required = field.Required
				} else {
					listType := field.Type.(map[string]interface{})
					icebergTableField.Type = listType["element"].(string)
					icebergTableField.Required = listType["element-required"].(bool)
					icebergTableField.IsList = true
				}

				icebergTableFields = append(icebergTableFields, icebergTableField)
			}
		}
	}

	return icebergTableFields, nil
}

func (storage *StorageUtils) WriteParquetFile(fileWriter source.ParquetFile, pgSchemaColumns []PgSchemaColumn, loadRows func() [][]string) (recordCount int64, err error) {
	defer fileWriter.Close()

	schemaMap := map[string]interface{}{
		"Tag":    "name=root",
		"Fields": []map[string]interface{}{},
	}
	for _, pgSchemaColumn := range pgSchemaColumns {
		fieldMap := pgSchemaColumn.ToParquetSchemaFieldMap()
		schemaMap["Fields"] = append(schemaMap["Fields"].([]map[string]interface{}), fieldMap)
	}
	schemaJson, err := json.Marshal(schemaMap)
	PanicIfError(err, storage.config)

	LogDebug(storage.config, "Parquet schema:", string(schemaJson))
	parquetWriter, err := writer.NewJSONWriter(string(schemaJson), fileWriter, PARQUET_PARALLEL_NUMBER)
	if err != nil {
		return 0, fmt.Errorf("failed to create Parquet writer: %v", err)
	}

	parquetWriter.RowGroupSize = PARQUET_ROW_GROUP_SIZE
	parquetWriter.CompressionType = PARQUET_COMPRESSION_TYPE

	rows := loadRows()
	for len(rows) > 0 {
		for _, row := range rows {
			rowMap := make(map[string]interface{})
			for i, rowValue := range row {
				rowMap[pgSchemaColumns[i].ColumnName] = pgSchemaColumns[i].FormatParquetValue(rowValue)
			}
			rowJson, err := json.Marshal(rowMap)
			PanicIfError(err, storage.config)

			if err = parquetWriter.Write(string(rowJson)); err != nil {
				return 0, fmt.Errorf("Write error: %v", err)
			}
			recordCount++
		}

		rows = loadRows()
	}

	LogDebug(storage.config, "Stopping Parquet writer...")
	if err := parquetWriter.WriteStop(); err != nil {
		return 0, fmt.Errorf("failed to stop Parquet writer: %v", err)
	}

	return recordCount, nil
}

func (storage *StorageUtils) ReadParquetStats(fileReader source.ParquetFile) (parquetFileStats ParquetFileStats, err error) {
	defer fileReader.Close()

	pr, err := reader.NewParquetReader(fileReader, nil, 1)
	if err != nil {
		return ParquetFileStats{}, fmt.Errorf("failed to create Parquet reader: %v", err)
	}
	defer pr.ReadStop()

	parquetStats := ParquetFileStats{
		ColumnSizes:     make(map[int]int64),
		ValueCounts:     make(map[int]int64),
		NullValueCounts: make(map[int]int64),
		LowerBounds:     make(map[int][]byte),
		UpperBounds:     make(map[int][]byte),
		SplitOffsets:    []int64{},
	}

	fieldIDMap := storage.buildFieldIDMap(pr.SchemaHandler)

	for _, rowGroup := range pr.Footer.RowGroups {
		if rowGroup.FileOffset != nil {
			parquetStats.SplitOffsets = append(parquetStats.SplitOffsets, *rowGroup.FileOffset)
		}

		for _, columnChunk := range rowGroup.Columns {
			columnMetaData := columnChunk.MetaData
			columnPath := columnMetaData.PathInSchema
			columnName := strings.Join(columnPath, ".")
			fieldID, ok := fieldIDMap[columnName]
			if !ok {
				continue
			}
			parquetStats.ColumnSizes[fieldID] += columnMetaData.TotalCompressedSize
			parquetStats.ValueCounts[fieldID] += int64(columnMetaData.NumValues)

			if columnMetaData.Statistics != nil {
				if columnMetaData.Statistics.NullCount != nil {
					parquetStats.NullValueCounts[fieldID] += *columnMetaData.Statistics.NullCount
				}

				minValue := columnMetaData.Statistics.Min
				maxValue := columnMetaData.Statistics.Max

				if parquetStats.LowerBounds[fieldID] == nil || bytes.Compare(parquetStats.LowerBounds[fieldID], minValue) > 0 {
					parquetStats.LowerBounds[fieldID] = minValue
				}
				if parquetStats.UpperBounds[fieldID] == nil || bytes.Compare(parquetStats.UpperBounds[fieldID], maxValue) < 0 {
					parquetStats.UpperBounds[fieldID] = maxValue
				}
			}
		}
	}

	// Todo: convert lower/upper bytes to BigEndianBytes?

	return parquetStats, nil
}

func (storage *StorageUtils) WriteManifestFile(fileSystemPrefix string, filePath string, parquetFile ParquetFile) (manifestFile ManifestFile, err error) {
	snapshotId := time.Now().UnixNano()
	codec, err := goavro.NewCodec(MANIFEST_SCHEMA)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("failed to create Avro codec: %v", err)
	}

	columnSizesArr := []interface{}{}
	for fieldID, size := range parquetFile.Stats.ColumnSizes {
		columnSizesArr = append(columnSizesArr, map[string]interface{}{
			"key":   fieldID,
			"value": size,
		})
	}

	valueCountsArr := []interface{}{}
	for fieldID, count := range parquetFile.Stats.ValueCounts {
		valueCountsArr = append(valueCountsArr, map[string]interface{}{
			"key":   fieldID,
			"value": count,
		})
	}

	nullValueCountsArr := []interface{}{}
	for fieldID, count := range parquetFile.Stats.NullValueCounts {
		nullValueCountsArr = append(nullValueCountsArr, map[string]interface{}{
			"key":   fieldID,
			"value": count,
		})
	}

	lowerBoundsArr := []interface{}{}
	for fieldID, value := range parquetFile.Stats.LowerBounds {
		lowerBoundsArr = append(lowerBoundsArr, map[string]interface{}{
			"key":   fieldID,
			"value": value,
		})
	}

	upperBoundsArr := []interface{}{}
	for fieldID, value := range parquetFile.Stats.UpperBounds {
		upperBoundsArr = append(upperBoundsArr, map[string]interface{}{
			"key":   fieldID,
			"value": value,
		})
	}

	dataFile := map[string]interface{}{
		"content":     0, // 0: DATA, 1: POSITION DELETES, 2: EQUALITY DELETES
		"file_path":   fileSystemPrefix + parquetFile.Path,
		"file_format": "PARQUET",
		// TODO: figure out "partition": ...
		"record_count":       parquetFile.RecordCount,
		"file_size_in_bytes": parquetFile.Size,
		"column_sizes": map[string]interface{}{
			"array": columnSizesArr,
		},
		"value_counts": map[string]interface{}{
			"array": valueCountsArr,
		},
		"null_value_counts": map[string]interface{}{
			"array": nullValueCountsArr,
		},
		"nan_value_counts": map[string]interface{}{
			"array": []interface{}{},
		},
		"lower_bounds": map[string]interface{}{
			"array": lowerBoundsArr,
		},
		"upper_bounds": map[string]interface{}{
			"array": upperBoundsArr,
		},
		"key_metadata": nil,
		"split_offsets": map[string]interface{}{
			"array": parquetFile.Stats.SplitOffsets,
		},
		"equality_ids":  nil,
		"sort_order_id": nil,
	}

	manifestEntry := map[string]interface{}{
		"status":               1, // 0: EXISTING 1: ADDED 2: DELETED
		"snapshot_id":          map[string]interface{}{"long": snapshotId},
		"sequence_number":      nil,
		"file_sequence_number": nil,
		"data_file":            dataFile,
	}

	avroFile, err := os.Create(filePath)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("failed to create manifest file: %v", err)
	}
	defer avroFile.Close()

	ocfWriter, err := goavro.NewOCFWriter(goavro.OCFConfig{
		W:      avroFile,
		Codec:  codec,
		Schema: MANIFEST_SCHEMA,
	})
	if err != nil {
		return ManifestFile{}, fmt.Errorf("failed to create Avro OCF writer: %v", err)
	}

	err = ocfWriter.Append([]interface{}{manifestEntry})
	if err != nil {
		return ManifestFile{}, fmt.Errorf("failed to write to manifest file: %v", err)
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("failed to get manifest file info: %v", err)
	}
	fileSize := fileInfo.Size()

	return ManifestFile{
		SnapshotId: snapshotId,
		Path:       filePath,
		Size:       fileSize,
	}, nil
}

func (storage *StorageUtils) WriteManifestListFile(fileSystemPrefix string, filePath string, parquetFile ParquetFile, manifestFile ManifestFile) (err error) {
	codec, err := goavro.NewCodec(MANIFEST_LIST_SCHEMA)
	if err != nil {
		return fmt.Errorf("failed to create Avro codec for manifest list: %v", err)
	}

	manifestListRecord := map[string]interface{}{
		"added_files_count":    1,
		"added_rows_count":     parquetFile.RecordCount,
		"added_snapshot_id":    manifestFile.SnapshotId,
		"content":              0,
		"deleted_files_count":  0,
		"deleted_rows_count":   0,
		"existing_files_count": 0,
		"existing_rows_count":  0,
		"key_metadata":         nil,
		"manifest_length":      manifestFile.Size,
		"manifest_path":        fileSystemPrefix + manifestFile.Path,
		"min_sequence_number":  1,
		"partition_spec_id":    0,
		"partitions":           map[string]interface{}{"array": []string{}},
		"sequence_number":      1,
	}

	avroFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create manifest list file: %v", err)
	}
	defer avroFile.Close()

	ocfWriter, err := goavro.NewOCFWriter(goavro.OCFConfig{
		W:      avroFile,
		Codec:  codec,
		Schema: MANIFEST_LIST_SCHEMA,
	})
	if err != nil {
		return fmt.Errorf("failed to create OCF writer for manifest list: %v", err)
	}

	err = ocfWriter.Append([]interface{}{manifestListRecord})
	if err != nil {
		return fmt.Errorf("failed to write manifest list record: %v", err)
	}

	return nil
}

func (storage *StorageUtils) WriteMetadataFile(fileSystemPrefix string, filePath string, pgSchemaColumns []PgSchemaColumn, parquetFile ParquetFile, manifestFile ManifestFile, manifestListFile ManifestListFile) (err error) {
	tableUuid := uuid.New().String()
	lastColumnID := 3
	currentTimestampMs := time.Now().UnixNano() / int64(time.Millisecond)

	icebergSchemaFields := make([]interface{}, len(pgSchemaColumns))
	for i, pgSchemaColumn := range pgSchemaColumns {
		icebergSchemaFields[i] = pgSchemaColumn.ToIcebergSchemaFieldMap()
	}

	metadata := map[string]interface{}{
		"format-version":       2,
		"table-uuid":           tableUuid,
		"location":             fileSystemPrefix + filePath,
		"last-sequence-number": 1,
		"last-updated-ms":      currentTimestampMs,
		"last-column-id":       lastColumnID,
		"schemas": []interface{}{
			map[string]interface{}{
				"type":                 "struct",
				"schema-id":            0,
				"fields":               icebergSchemaFields,
				"identifier-field-ids": []interface{}{},
			},
		},
		"current-schema-id": 0,
		"partition-specs": []interface{}{
			map[string]interface{}{
				"spec-id": 0,
				"fields":  []interface{}{},
			},
		},
		"default-spec-id":       0,
		"default-sort-order-id": 0,
		"last-partition-id":     999, // Assuming no partitions; set to a placeholder
		"properties":            map[string]string{},
		"current-snapshot-id":   manifestFile.SnapshotId,
		"refs": map[string]interface{}{
			"main": map[string]interface{}{
				"snapshot-id": manifestFile.SnapshotId,
				"type":        "branch",
			},
		},
		"snapshots": []interface{}{
			map[string]interface{}{
				"schema-id":       0,
				"snapshot-id":     manifestFile.SnapshotId,
				"sequence-number": 1,
				"timestamp-ms":    currentTimestampMs,
				"manifest-list":   fileSystemPrefix + manifestListFile.Path,
				"summary": map[string]interface{}{
					"added-data-files":       "1",
					"added-files-size":       strconv.FormatInt(parquetFile.Size, 10),
					"added-records":          strconv.FormatInt(parquetFile.RecordCount, 10),
					"operation":              "append",
					"total-data-files":       "1",
					"total-delete-files":     "0",
					"total-equality-deletes": "0",
					"total-files-size":       strconv.FormatInt(parquetFile.Size, 10),
					"total-position-deletes": "0",
					"total-records":          strconv.FormatInt(parquetFile.RecordCount, 10),
				},
			},
		},
		"snapshot-log": []interface{}{
			map[string]interface{}{
				"snapshot-id":  manifestFile.SnapshotId,
				"timestamp-ms": currentTimestampMs,
			},
		},
		"metadata-log": []interface{}{},
		"sort-orders": []interface{}{
			map[string]interface{}{
				"order-id": 0,
				"fields":   []interface{}{},
			},
		},
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create metadata file: %v", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(metadata)
	if err != nil {
		return fmt.Errorf("failed to write metadata to file: %v", err)
	}

	return nil
}

func (storage *StorageUtils) WriteVersionHintFile(filePath string, metadataFile MetadataFile) (err error) {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create version hint file: %v", err)
	}
	defer file.Close()

	_, err = file.WriteString(fmt.Sprintf("%d", metadataFile.Version))
	if err != nil {
		return fmt.Errorf("failed to write to version hint file: %v", err)
	}

	return nil
}

func (storage *StorageUtils) ParseInternalTableMetadata(metadataContent []byte) (InternalTableMetadata, error) {
	var internalTableMetadata InternalTableMetadata
	err := json.Unmarshal(metadataContent, &internalTableMetadata)
	if err != nil {
		return InternalTableMetadata{}, err
	}
	return internalTableMetadata, nil
}

func (storage *StorageUtils) WriteInternalTableMetadataFile(filePath string, internalTableMetadata InternalTableMetadata) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create internal table metadata file: %v", err)
	}
	defer file.Close()

	jsonData, err := json.Marshal(internalTableMetadata)
	if err != nil {
		return fmt.Errorf("failed to serialize internal table metadata to JSON: %v", err)
	}

	_, err = file.Write(jsonData)
	if err != nil {
		return fmt.Errorf("failed to write internal table metadata to file: %v", err)
	}

	return nil

}

func (storage *StorageUtils) buildFieldIDMap(schemaHandler *schema.SchemaHandler) map[string]int {
	fieldIDMap := make(map[string]int)
	for _, schema := range schemaHandler.SchemaElements {
		if schema.FieldID != nil {
			fieldIDMap[schema.Name] = int(*schema.FieldID)
		}
	}
	return fieldIDMap
}
