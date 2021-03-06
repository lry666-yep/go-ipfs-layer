package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"os"

	u "github.com/ipfs/go-ipfs-util"
)

const (
	pkgName        = "asnutil"
	ipv6OutputFile = "ipv6_asn_map.go"
	ipv6MapName    = "ipv6CidrToAsnMap"
)

func main() {
	// file with the ASN mappings for IPv6 CIDRs.
	// See ipv6_asn.tsv
	ipv6File := os.Getenv("ASN_IPV6_FILE")

	if len(ipv6File) == 0 {
		panic(errors.New("environment vars must be provided"))
	}

	ipv6CidrToAsnMap := readMappingFile(ipv6File)
	f, err := os.Create(ipv6OutputFile)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	writeMappingToFile(f, ipv6CidrToAsnMap, ipv6MapName)
}

func writeMappingToFile(f *os.File, m map[string]string, mapName string) {
	printf := func(s string, args ...interface{}) {
		_, err := fmt.Fprintf(f, s, args...)
		if err != nil {
			panic(err)
		}
	}
	printf("package %s\n\n", pkgName)
	printf("// Code generated by generate/main.go DO NOT EDIT\n")
	printf("var %s = map[string]string {", mapName)
	for k, v := range m {
		printf("\n\t \"%s\": \"%s\",", k, v)
	}
	printf("\n}")
}

func readMappingFile(path string) map[string]string {
	m := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = '\t'
	for {
		record, err := r.Read()
		// Stop at EOF.
		if err == io.EOF {
			return m
		}

		startIP := record[0]
		endIP := record[1]
		asn := record[2]
		if asn == "0" {
			continue
		}

		s := net.ParseIP(startIP)
		e := net.ParseIP(endIP)
		if s.To16() == nil || e.To16() == nil {
			panic(errors.New("IP should be v6"))
		}

		prefixLen := zeroPrefixLen(u.XOR(s.To16(), e.To16()))
		cn := fmt.Sprintf("%s/%d", startIP, prefixLen)
		m[cn] = asn
	}

}

func zeroPrefixLen(id []byte) int {
	for i, b := range id {
		if b != 0 {
			return i*8 + bits.LeadingZeros8(uint8(b))
		}
	}
	return len(id) * 8
}
