package eos

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

func (a *ABI) DecodeAction(data []byte, actionName ActionName) ([]byte, error) {
	binaryDecoder := NewDecoder(data)
	action := a.ActionForName(actionName)
	if action == nil {
		return nil, fmt.Errorf("action %s not found in abi", actionName)
	}

	return a.decode(binaryDecoder, action.Type)

}

func (a *ABI) DecodeTableRow(tableName TableName, data []byte) ([]byte, error) {
	binaryDecoder := NewDecoder(data)
	tbl := a.TableForName(tableName)
	if tbl == nil {
		return nil, fmt.Errorf("table name %s not found in abi", tableName)
	}

	return a.decode(binaryDecoder, tbl.Type)

}

func (a *ABI) DecodeTableRowTyped(tableType string, data []byte) ([]byte, error) {
	binaryDecoder := NewDecoder(data)
	return a.decode(binaryDecoder, tableType)
}

func (a *ABI) Decode(binaryDecoder *Decoder, structName string) ([]byte, error) {
	return a.decode(binaryDecoder, structName)
}

func (a *ABI) decode(binaryDecoder *Decoder, structName string) ([]byte, error) {
	if loggingEnabled {
		abiDecoderLog.Debug("decode struct", zap.String("name", structName))
	}

	structure := a.StructForName(structName)
	if structure == nil {
		return nil, fmt.Errorf("structure [%s] not found in abi", structName)
	}

	resultingJSON := make([]byte, 0)
	if structure.Base != "" {
		if loggingEnabled {
			abiDecoderLog.Debug("struct has base struct", zap.String("name", structName), zap.String("base", structure.Base))
		}

		baseName, isAlias := a.TypeNameForNewTypeName(structure.Base)
		if isAlias && loggingEnabled {
			abiDecoderLog.Debug("base is an alias", zap.String("from", structure.Base), zap.String("to", baseName))
		}

		var err error
		resultingJSON, err = a.decode(binaryDecoder, baseName)
		if err != nil {
			return nil, fmt.Errorf("decode base [%s]: %s", structName, err)
		}
	}

	return a.decodeFields(binaryDecoder, structure.Fields, resultingJSON)
}

func (a *ABI) decodeFields(binaryDecoder *Decoder, fields []FieldDef, json []byte) ([]byte, error) {
	resultingJSON := json
	for _, field := range fields {
		fieldType, isOptional, isArray, isBinaryExtension := analyzeFieldType(field.Type)
		if isBinaryExtension && !binaryDecoder.hasRemaining() {
			if loggingEnabled {
				abiDecoderLog.Debug("type is a binary extension and no more data, skipping field", zap.String("type", field.Type))
			}
			continue
		}

		typeName, isAlias := a.TypeNameForNewTypeName(fieldType)
		if isAlias {
			if loggingEnabled {
				abiDecoderLog.Debug("type is an alias", zap.String("from", field.Type), zap.String("to", typeName))
			}
		}

		var err error
		resultingJSON, err = a.decodeField(binaryDecoder, field.Name, typeName, isOptional, isArray, resultingJSON)
		if err != nil {
			return nil, fmt.Errorf("decoding fields: %s", err)
		}
	}

	return resultingJSON, nil
}

func (a *ABI) decodeField(binaryDecoder *Decoder, fieldName string, fieldType string, isOptional bool, isArray bool, json []byte) ([]byte, error) {
	if loggingEnabled {
		abiDecoderLog.Debug("decoding field", zap.String("name", fieldName), zap.String("type", fieldType), zap.Bool("is_optional", isOptional), zap.Bool("is_array", isArray))
	}

	resultingJSON := json
	if isOptional {

		if loggingEnabled {
			abiDecoderLog.Debug("field is optional", zap.String("name", fieldName))
		}
		b, err := binaryDecoder.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("decoding field [%s] optional flag: %s", fieldName, err)
		}

		if b == 0 {
			if loggingEnabled {
				abiDecoderLog.Debug("field is not present", zap.String("name", fieldName))
			}
			if !a.fitNodeos {
				return resultingJSON, nil
			}
			fieldType = "null"
		}
	}

	if isArray {
		length, err := binaryDecoder.ReadUvarint64()
		if err != nil {
			return nil, fmt.Errorf("reading field [%s] array length: %s", fieldName, err)
		}

		if length == 0 {
			resultingJSON, _ = sjson.SetBytes(resultingJSON, fieldName, []interface{}{})
			//ignoring err because there is a bug in sjson. sjson shadow the err in case of a default type ...
			//if err != nil {
			//	return nil, fmt.Errorf("reading field [%s] setting empty array: %s", fieldName, err)
			//}
		}

		for i := uint64(0); i < length; i++ {
			if loggingEnabled {
				abiDecoderLog.Debug("adding value for field", zap.String("name", fieldName), zap.Uint64("index", i))
			}
			indexedFieldName := fmt.Sprintf("%s.%d", fieldName, i)
			resultingJSON, err = a.read(binaryDecoder, indexedFieldName, fieldType, resultingJSON)
			if err != nil {
				return nil, fmt.Errorf("reading field [%s] index [%d]: %s", fieldName, i, err)
			}
		}

		return resultingJSON, nil
	}

	resultingJSON, err := a.read(binaryDecoder, fieldName, fieldType, resultingJSON)
	if err != nil {
		return nil, fmt.Errorf("decoding field [%s] of type [%s]: %s", fieldName, fieldType, err)
	}

	return resultingJSON, nil
}

func (a *ABI) read(binaryDecoder *Decoder, fieldName string, fieldType string, json []byte) ([]byte, error) {
	variant := a.VariantForName(fieldType)
	if variant != nil {
		variantIndex, err := binaryDecoder.ReadUvarint32()
		if err != nil {
			return nil, fmt.Errorf("unable to read variant type index: %s", err)
		}

		if int(variantIndex) >= len(variant.Types) {
			return nil, fmt.Errorf("variant type index is unknown, got type index %d, know up to index %d", variantIndex, len(variant.Types)-1)
		}

		variantFieldType := variant.Types[variantIndex]
		if loggingEnabled {
			abiDecoderLog.Debug("field is a variant", zap.String("type", variantFieldType))
		}

		resolvedVariantFieldType, isAlias := a.TypeNameForNewTypeName(variantFieldType)
		if isAlias && loggingEnabled {
			abiDecoderLog.Debug("variant type is an alias", zap.String("from", fieldType), zap.String("to", resolvedVariantFieldType))
		}

		fieldType = resolvedVariantFieldType
	}

	structure := a.StructForName(fieldType)

	if structure != nil {
		if loggingEnabled {
			abiDecoderLog.Debug("field is a struct", zap.String("name", fieldName))
		}

		structureJSON := []byte{}
		if structure.Base != "" {
			if loggingEnabled {
				abiDecoderLog.Debug("field's struct has base struct", zap.String("name", fieldName), zap.String("struct", structure.Name), zap.String("base", structure.Base))
			}

			baseName, isAlias := a.TypeNameForNewTypeName(structure.Base)
			if isAlias {
				if loggingEnabled {
					abiDecoderLog.Debug("base is an alias", zap.String("from", structure.Base), zap.String("to", baseName))
				}
			}

			baseStructure := a.StructForName(baseName)
			if baseStructure == nil {
				return nil, fmt.Errorf("base structure [%s] not found in abi", baseName)
			}

			var err error
			structureJSON, err = a.decodeFields(binaryDecoder, baseStructure.Fields, structureJSON)
			if err != nil {
				return nil, fmt.Errorf("decoding field [%s] struct [%s] base [%s]: %s", fieldName, structure.Name, baseName, err)
			}
		}

		var err error
		structureJSON, err = a.decodeFields(binaryDecoder, structure.Fields, structureJSON)
		if err != nil {
			return nil, err
		}

		if loggingEnabled {
			abiDecoderLog.Debug("set field value", zap.String("name", fieldName), zap.ByteString("json", structureJSON))
		}
		return sjson.SetRawBytes(json, fieldName, structureJSON)
	}

	var value interface{}
	var err error
	switch fieldType {
	case "null":
		value = nil
	case "int8":
		value, err = binaryDecoder.ReadInt8()
	case "uint8":
		value, err = binaryDecoder.ReadUInt8()
	case "int16":
		value, err = binaryDecoder.ReadInt16()
	case "uint16":
		value, err = binaryDecoder.ReadUint16()
	case "int32":
		value, err = binaryDecoder.ReadInt32()
	case "uint32":
		value, err = binaryDecoder.ReadUint32()
	case "int64":
		var val int64
		val, err = binaryDecoder.ReadInt64()
		value = Int64(val)
	case "uint64":
		var val uint64
		val, err = binaryDecoder.ReadUint64()
		value = Uint64(val)
	case "int128":
		value, err = binaryDecoder.ReadUint128("int128")
	case "uint128":
		value, err = binaryDecoder.ReadUint128("uint128")
	case "varint32":
		value, err = binaryDecoder.ReadVarint32()
	case "varuint32":
		value, err = binaryDecoder.ReadUvarint32()
	case "float32":
		value, err = binaryDecoder.ReadFloat32()
	case "float64":
		v, e := binaryDecoder.ReadFloat64()
		if e == nil {
			if a.fitNodeos {
				value = strconv.FormatFloat(v, 'f', 17, 64)
				break
			}
		}
		value = v
		err = e
	case "float128":
		value, err = binaryDecoder.ReadUint128("float128")
	case "bool":
		if a.fitNodeos {
			value, err = binaryDecoder.ReadByte()
			break
		}
		value, err = binaryDecoder.ReadBool()
	case "time_point":
		timePoint, e := binaryDecoder.ReadTimePoint() //todo double check
		if e == nil {
			t := time.Unix(0, int64(timePoint*1000))
			value = t.UTC().Format("2006-01-02T15:04:05.999")
		}
		err = e
	case "time_point_sec":
		timePointSec, e := binaryDecoder.ReadTimePointSec()
		if e == nil {
			t := time.Unix(int64(timePointSec), 0)
			value = t.UTC().Format("2006-01-02T15:04:05")
		}
		err = e
	case "block_timestamp_type":
		value, err = binaryDecoder.ReadBlockTimestamp()
		if err == nil {
			value = value.(BlockTimestamp).Time.UTC().Format("2006-01-02T15:04:05")
		}
	case "name":
		value, err = binaryDecoder.ReadName()
	case "bytes":
		value, err = binaryDecoder.ReadByteArray()
		if err == nil {
			value = hex.EncodeToString(value.([]byte))
		}
	case "string":
		value, err = binaryDecoder.ReadString()
	case "checksum160":
		value, err = binaryDecoder.ReadChecksum160()
	case "checksum256":
		value, err = binaryDecoder.ReadChecksum256()
	case "checksum512":
		value, err = binaryDecoder.ReadChecksum512()
	case "public_key":
		value, err = binaryDecoder.ReadPublicKey()
	case "signature":
		value, err = binaryDecoder.ReadSignature()
	case "symbol":

		symbol, e := binaryDecoder.ReadSymbol()
		err = e
		if err == nil {
			value = fmt.Sprintf("%d,%s", symbol.Precision, symbol.Symbol)
		}

	case "symbol_code":
		value, err = binaryDecoder.ReadSymbolCode()
	case "asset":
		value, err = binaryDecoder.ReadAsset()
	case "extended_asset":
		value, err = binaryDecoder.ReadExtendedAsset()
	default:
		return nil, fmt.Errorf("read field of type [%s]: unknown type", fieldType)
	}

	if err != nil {
		return nil, fmt.Errorf("read: %s", err)
	}

	if loggingEnabled {
		abiDecoderLog.Debug("set field value", zap.String("name", fieldName), zap.Reflect("value", value))
	}

	return sjson.SetBytes(json, fieldName, value)
}

func analyzeFieldType(fieldType string) (typeName string, isOptional bool, isArray bool, isBinaryExtension bool) {
	if strings.HasSuffix(fieldType, "?") {
		return fieldType[0 : len(fieldType)-1], true, false, false
	}

	if strings.HasSuffix(fieldType, "$") {
		return fieldType[0 : len(fieldType)-1], false, false, true
	}

	if strings.HasSuffix(fieldType, "[]") {
		return fieldType[0 : len(fieldType)-2], false, true, false
	}

	return fieldType, false, false, false
}