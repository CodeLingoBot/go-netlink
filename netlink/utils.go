package netlink

import (
	"os"
	"fmt"
	"net"
	"encoding/binary"
	"reflect"
	"bytes"
	"syscall"
)

func netlinkPadding(size int) int {
	partialChunk := size % syscall.NLMSG_ALIGNTO
	return (syscall.NLMSG_ALIGNTO - partialChunk) % syscall.NLMSG_ALIGNTO
}

func skipAlignedFromSlice(r *bytes.Buffer, dataLen int) os.Error {
	r.Next(dataLen + netlinkPadding(dataLen))
	return nil
}

func strtoi(s string) int {
	i := 0
	for _, c := range s {
		i *= 10
		i += c - '0'
	}
	return i
}

// Returns pointer to a field, and type information corresponding to a given numerical ID.
func getDestinationAndType(object interface{}, id uint16) (reflect.Value, string, os.Error) {
	ptrType := reflect.TypeOf(object)
	// check the object is a pointer
	if ptrType.Kind() != reflect.Ptr {
		er := fmt.Errorf("getDestinationAndType() received"+
			" object of Kind %s, expected pointer!", ptrType.Kind())
		return reflect.ValueOf(nil), "", er
	}
	// check the indirected object is a struct
	objType := ptrType.Elem()
	if objType.Kind() != reflect.Struct {
		er := fmt.Errorf("getDestinationAndType() received"+
			" a pointer to %s, expected pointer to struct!", objType.Kind())
		return reflect.ValueOf(nil), "", er
	}
	// find appropriate field
	for i := 0; i < objType.NumField(); i++ {
		objField := objType.Field(i)
		if strtoi(objField.Tag.Get("netlink")) == int(id) {
			// found field
			type_s := objField.Tag.Get("type")
			// returns ValueOf((*object).field)
			fieldValue := reflect.Indirect(reflect.ValueOf(object)).Field(i)
			return fieldValue, type_s, nil
		}
	}
	er := fmt.Errorf("could not find field ID %d in object of type %s",
		id, objType)
	return reflect.ValueOf(nil), "", er
}

// Reads one attribute into a structure.
// dest must be a pointer to a struct.
func readAttribute(r *bytes.Buffer, dest interface{}) (er os.Error) {
	var attr syscall.RtAttr
	er = binary.Read(r, systemEndianness, &attr)
	if er != nil {
		return er
	}
	dataLen := int(attr.Len) - syscall.SizeofRtAttr
	value, type_spec, er := getDestinationAndType(dest, attr.Type)
	switch true {
	case er != nil:
		return er
	case type_spec == "fixed":
		if !value.CanAddr() {
			return fmt.Errorf("trying to read fixed-width data in a non addressable field!")
		}
		er = binary.Read(r, systemEndianness, value.Addr().Interface())
	case type_spec == "bytes":
		buf := make([]byte, dataLen)
		_, er = r.Read(buf[:])
		value.Set(reflect.ValueOf(buf))
	case type_spec == "string":
		// Reads a NUL-terminated byte array
		if value.Type().Kind() != reflect.String {
			return fmt.Errorf("unable to fill field of type %s with string!", value.Type())
		}
		buf := make([]byte, dataLen)
		_, er = r.Read(buf[:])
		s := string(buf[:len(buf)-1])
		value.Set(reflect.ValueOf(s))
	case type_spec == "nested":
		if !value.CanAddr() {
			return fmt.Errorf("trying to read nested attributes to a non addressable field!")
		}
		buf := make([]byte, dataLen)
		_, er = r.Read(buf[:])
		er = readManyAttributes(bytes.NewBuffer(buf), value.Addr().Interface())
	case type_spec == "nestedlist":
		buf := make([]byte, dataLen)
		_, er = r.Read(buf[:])
		er = readNestedAttributeList(bytes.NewBuffer(buf), value)
	default:
		return fmt.Errorf("Invalid format tag %s: expecting 'fixed', 'bytes', 'string', or 'nested'", type_spec)
	}
	r.Next(netlinkPadding(dataLen))
	return er
}

func readManyAttributes(r *bytes.Buffer, dest interface{}) (er os.Error) {
	for {
		er := readAttribute(r, dest)
		switch er {
		case nil:
			break
		case os.EOF:
			return nil
		default:
			return er
		}
	}
	return nil
}

// Reads n nested attributes into the elements of an array
func readNestedAttributeList(r *bytes.Buffer, dest reflect.Value) (er os.Error) {
	if dest.Type().Kind() != reflect.Slice {
		return fmt.Errorf("unable to fill field of type %s with list of nested attrs!", dest.Type())
	}
	for {
		var attr syscall.RtAttr
		er = binary.Read(r, systemEndianness, &attr)
		switch er {
		case nil:
			break
		case os.EOF:
			return nil
		default:
			return er
		}
		dataLen := int(attr.Len) - syscall.SizeofRtAttr

		// Create buffer for nested attribute
		buf := make([]byte, dataLen)
		_, er = r.Read(buf[:])

		// Read the value
		item := reflect.New(dest.Type().Elem())
		er = readManyAttributes(bytes.NewBuffer(buf), item.Interface())

		// Append the value
		dest.Set(reflect.Append(dest, reflect.Indirect(item)))
	}
	return nil
}

func readNestedFromSlice(attr []byte, data *[][]byte) os.Error {
	buf := bytes.NewBuffer(attr)
	for {
		var attr syscall.RtAttr
		var subattr []byte
		er := binary.Read(buf, systemEndianness, &attr)
		dataLen := int(attr.Len) - syscall.SizeofRtAttr
		switch true {
		case er == os.EOF:
			return nil
		case dataLen > buf.Len():
			return fmt.Errorf("invalid attribute length: %d > %d", dataLen, buf.Len())
		case er != nil:
			return er
		}
		readAlignedFromSlice(buf, &subattr, dataLen)
		*data = append(*data, subattr)
	}
	return nil
}

func readAlignedFromSlice(r *bytes.Buffer, data interface{}, dataLen int) os.Error {
	var er os.Error
	switch dest := data.(type) {
	case nil:
		r.Next(dataLen)
	case *[]byte:
		*dest = make([]byte, dataLen)
		_, er = r.Read((*dest)[:])
	case *net.IP:
		*dest = make([]byte, dataLen)
		_, er = r.Read((*dest)[:])
	case *string:
		// Read a NULL-terminated string 
		buffer := make([]byte, dataLen)
		_, er = r.Read(buffer[:])
		*dest = string(buffer[:len(buffer)-1])
	default:
		// Read a binary struct
		er = binary.Read(r, systemEndianness, data)
		realLen := sizeof(data)
		r.Next(dataLen - realLen)
	}
	if er != nil {
		return er
	}
	// advance by the padding size
	r.Next(netlinkPadding(dataLen))
	return nil
}

func putAttribute(w *bytes.Buffer, attrtype uint16, data interface{}) os.Error {
	var attr Attr
	switch data := data.(type) {
	case []byte:
		attr = Attr{Len: uint16(len(data)), Type: attrtype}
		binary.Write(w, systemEndianness, attr)
		binary.Write(w, systemEndianness, data)
	case string:
		attr = Attr{Len: uint16(len(data) + 1), Type: attrtype}
		binary.Write(w, systemEndianness, attr)
		binary.Write(w, systemEndianness, []byte(data))
		w.WriteByte(0)
	default:
		attr = Attr{Len: uint16(sizeof(data)), Type: attrtype}
		binary.Write(w, systemEndianness, attr)
		binary.Write(w, systemEndianness, data)
	}
	for i := 0; i < netlinkPadding(int(attr.Len)); i++ {
		w.WriteByte(0)
	}
	return nil
}

func sizeof(data interface{}) int {
	var v reflect.Value
	switch d := reflect.ValueOf(data); d.Kind() {
	case reflect.Ptr:
		v = d.Elem()
	case reflect.Slice:
		v = d
	default:
		v = d
	}
	return binary.TotalSize(v)
}
