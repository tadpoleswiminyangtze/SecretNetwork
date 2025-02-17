package remote_attestation

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"unsafe"

	"github.com/pkg/errors"
	"golang.org/x/xerrors"
)

type CombinedHdr struct {
	M_CombinedSizes [3]uint32
}

type DcapQuote struct {
	M_Opaque1 [48]byte  // sgx_quote_t up to report_body
	M_Opaque2 [320]byte // sgx_report_body_t up to report_ata
	M_PubKey  [32]byte
	M_Opaque3 [32]byte // remaining 32 bytes of report_data
	M_SigLen  uint32
}

func VerifyCombinedCert(blob []byte) ([]byte, error) {
	var hdr CombinedHdr

	if uintptr(len(blob)) < unsafe.Sizeof(hdr) {
		return nil, errors.New("Combined hdr too small")
	}

	{
		buf := bytes.NewReader(blob)
		err := binary.Read(buf, binary.LittleEndian, &hdr)
		if err != nil {
			return nil, err
		}
	}

	idx0 := unsafe.Sizeof(hdr)
	idx1 := idx0 + uintptr(hdr.M_CombinedSizes[0])
	idx2 := idx1 + uintptr(hdr.M_CombinedSizes[1])
	idx3 := idx2 + uintptr(hdr.M_CombinedSizes[2])

	if uintptr(len(blob)) < idx3 {
		return nil, errors.New("combined hdr invalid")
	}

	if idx1 > idx0 {
		ret_pk, ret_err := VerifyRaCert(blob[idx0:idx1])
		if ret_pk != nil {
			fmt.Println("EPID quote Extracted pk: ", hex.EncodeToString(ret_pk))
		}
		return ret_pk, ret_err
	}

	if idx2 > idx1 {
		var quote DcapQuote

		buf := bytes.NewReader(blob[idx1:idx2])
		err := binary.Read(buf, binary.LittleEndian, &quote)
		if err != nil {
			return nil, err
		}

		fmt.Println("DCAP quote Extracted pk: ", hex.EncodeToString(quote.M_PubKey[:]))
		return quote.M_PubKey[:], nil
	}

	return nil, errors.New("No valid attestatoin found")
}

/*
	 Verifies the remote attestation certificate, which is comprised of a the attestation report, intel signature, and enclave signature

	 We verify that:
		- the report is valid, that no outstanding issues exist (todo: match enclave hash or something?)
		- Intel's certificate signed the report
		- The public key of the enclave/node exists, so we can use that to encrypt the seed

	 In software mode this will just return the raw netscape comment, as it is the public key of the signer
*/
func VerifyRaCert(rawCert []byte) ([]byte, error) {
	// printCert(rawCert)
	// get the pubkey and payload from raw data

	pubK, payload, err := unmarshalCert(rawCert)
	if err != nil {
		return nil, xerrors.Errorf("Unmarshal certificate failed: %v", err)
	}

	if !isSgxHardwareMode() {
		pk, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil {
			return nil, xerrors.Errorf("Decode certificate failed: %v", err)
		}

		return pk, nil
	}

	// Load Intel CA, Verify Cert and Signature
	attnReportRaw, err := verifyCert(payload)
	if err != nil {
		return nil, xerrors.Errorf("Intel verification failed: %v", err)
	}

	// Verify attestation report
	pubK, err = verifyAttReport(attnReportRaw, pubK)
	if err != nil {
		return nil, xerrors.Errorf("Attestation report failed: %v", err)
	}
	// verifyAttReport returns all the report_data field, which is 64 bytes - we just want the first 32 of them (rest are 0)
	return pubK[0:32], nil
}

// UNSAFE_VerifyRaCert This function is a variant that should be used in the CLI - since parsing certificates is different in
// software or hardware modes, this function tries the HW route and goes with Software otherwise. Since there's no verification in
// SW mode it will return the 32 bytes of the public key it finds.
// TODO: a more elegant fix for this issue would be to return whether we are in HW or SW when querying for the tx key (although this could fail in offline modes, so maybe not)
func UNSAFE_VerifyRaCert(rawCert []byte) ([]byte, error) {
	// printCert(rawCert)
	// get the pubkey and payload from raw data

	pubK, payload, err := unmarshalCert(rawCert)
	if err != nil {
		return nil, err
	}

	// Load Intel CA, Verify Cert and Signature
	attnReportRaw, err := verifyCert(payload)
	if err != nil {
		pk, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil {
			return nil, err
		}

		if len(pk) != 32 {
			return nil, errors.New("Failed to parse certificate. Is node working?")
		}

		return pk, nil
	}

	// Verify attestation report
	pubK, err = verifyAttReport(attnReportRaw, pubK)
	if err != nil {
		return nil, err
	}
	// verifyAttReport returns all the report_data field, which is 64 bytes - we just want the first 32 of them (rest are 0)
	return pubK[0:32], nil
}

func extractAsn1Value(cert []byte, oid []byte) ([]byte, error) {
	offset := uint(bytes.Index(cert, oid))
	offset += 12 // 11 + TAG (0x04)

	// we will be accessing offset + 2, so make sure it's not out-of-bounds
	if offset+2 >= uint(len(cert)) {
		err := errors.New("Error parsing certificate - malformed certificate")
		return nil, err
	}

	// Obtain Netscape Comment length
	length := uint(cert[offset])
	if length > 0x80 {
		length = uint(cert[offset+1])*uint(0x100) + uint(cert[offset+2])
		offset += 2
	}

	if offset+length+1 >= uint(len(cert)) {
		err := errors.New("Error parsing certificate - malformed certificate")
		return nil, err
	}

	// Obtain Netscape Comment
	offset++
	payload := cert[offset : offset+length]

	return payload, nil
}

func extractPublicFromCert(cert []byte) ([]byte, error) {
	prime256v1Oid := []byte{0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07}
	return extractAsn1Value(cert, prime256v1Oid)
}

func unmarshalCert(rawbyte []byte) ([]byte, []byte, error) {
	// Search for Public Key prime256v1 OID
	// Obtain Public Key

	pubK, err := extractPublicFromCert(rawbyte)
	if err != nil {
		return nil, nil, err
	}
	// Search for Netscape Comment OID
	nsCmtOid := []byte{0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x86, 0xF8, 0x42, 0x01, 0x0D}
	payload, err := extractAsn1Value(rawbyte, nsCmtOid)
	if err != nil {
		return nil, nil, err
	}

	return pubK, payload, err
}

func verifyCert(payload []byte) ([]byte, error) {
	// Extract each field

	var signedReport EndorsedAttestationReport

	err := json.Unmarshal(payload, &signedReport)
	if err != nil {
		return nil, err
	}

	certServer, err := x509.ParseCertificate(signedReport.SigningCert)
	if err != nil {
		return nil, err
	}

	roots := x509.NewCertPool()

	ok := roots.AppendCertsFromPEM([]byte(rootIntelPEM))
	if !ok {
		panic("failed to parse root certificate")
	}

	opts := x509.VerifyOptions{
		Roots: roots,
		// note: there's no way to not validate the time, and we don't want to write this code
		// ourselves. We also can't just ignore the error message, since that means that the rest of
		// the validation didn't happen (time is validated early on)
		CurrentTime: time.Date(2023, 11, 0o4, 0o0, 0o0, 0o0, 0o0, time.UTC),
	}

	if _, err := certServer.Verify(opts); err != nil {
		return nil, err
	}

	// Verify the signature against the signing cert
	err = certServer.CheckSignature(certServer.SignatureAlgorithm, signedReport.Report, signedReport.Signature)
	if err != nil {
		return nil, err
	}

	return signedReport.Report, nil
}

func verifyAttReport(attnReportRaw []byte, pubK []byte) ([]byte, error) {
	var qr QuoteReport
	err := json.Unmarshal(attnReportRaw, &qr)
	if err != nil {
		return nil, err
	}

	// 1. Check timestamp is within 24H
	if qr.Timestamp == "" {
		return nil, errors.New("Failed to fetch timestamp from attestation report")
	}

	// 2. Verify quote status (mandatory field)

	if qr.IsvEnclaveQuoteStatus != "" {
		// fmt.Println("isvEnclaveQuoteStatus = ", qr.IsvEnclaveQuoteStatus)
		switch qr.IsvEnclaveQuoteStatus {
		case "OK":
			break
		case "GROUP_REVOKED", "CONFIGURATION_NEEDED", "CONFIGURATION_AND_SW_HARDENING_NEEDED":

			// Verify platformInfoBlob for further info if status not OK
			if qr.PlatformInfoBlob != "" {
				platInfo, err := hex.DecodeString(qr.PlatformInfoBlob)
				if err != nil && len(platInfo) != 105 {
					return nil, errors.New("illegal PlatformInfoBlob")
				}
				// platInfo = platInfo[4:]

				// piBlob := parsePlatform(platInfo)
				// piBlobJson, err := json.Marshal(piBlob)
				// if err != nil {
				//	return nil, err
				//}
				// fmt.Println("Platform info is: " + string(piBlobJson))
			} else {
				return nil, errors.New("Failed to fetch platformInfoBlob from attestation report")
			}
			if len(qr.AdvisoryIDs) != 0 {
				_, err := json.Marshal(qr.AdvisoryIDs)
				if err != nil {
					return nil, err
				}
			}
		case "SW_HARDENING_NEEDED", "GROUP_OUT_OF_DATE":
			if len(qr.AdvisoryIDs) != 0 {
				_, err := json.Marshal(qr.AdvisoryIDs)
				if err != nil {
					return nil, err
				}
				// fmt.Println("Advisory IDs: " + string(cves))
				// return nil, errors.New("Platform is vulnerable, and requires updates before authorization: " + string(cves))
			} else {
				return nil, errors.New("Failed to fetch advisory IDs even though platform is vulnerable")
			}

		default:
			return nil, errors.New("SGX_ERROR_UNEXPECTED")
		}
	} else {
		err := errors.New("Failed to fetch isvEnclaveQuoteStatus from attestation report")
		return nil, err
	}

	// 3. Verify quote body (mandatory field)
	if qr.IsvEnclaveQuoteBody != "" {
		qb, err := base64.StdEncoding.DecodeString(qr.IsvEnclaveQuoteBody)
		if err != nil {
			return nil, err
		}

		var quoteBytes, quoteHex, pubHex string
		for _, b := range qb {
			quoteBytes += fmt.Sprint(int(b), ", ")
			quoteHex += fmt.Sprintf("%02x", int(b))
		}

		for _, b := range pubK {
			pubHex += fmt.Sprintf("%02x", int(b))
		}

		qrData := parseReport(qb, quoteHex)

		// todo: possibly verify mr signer/enclave?
		// fmt.Println("Quote = [" + quoteBytes[:len(quoteBytes)-2] + "]")
		// fmt.Println("sgx quote version = ", qrData.version)
		// fmt.Println("sgx quote signature type = ", qrData.signType)
		// fmt.Println("sgx quote report_data = ", qrData.reportBody.reportData)
		// fmt.Println("sgx quote mr_enclave = ", qrData.reportBody.mrEnclave)
		// fmt.Println("sgx quote mr_signer = ", qrData.reportBody.mrSigner)
		// fmt.Println("Anticipated public key = ", pubHex)

		if qrData.ReportBody.ReportData != pubHex {
			// err := errors.New("Failed to authenticate certificate public key")
			reportPubKey, err := hex.DecodeString(qrData.ReportBody.ReportData)
			if err != nil {
				return nil, err
			}
			return reportPubKey, nil
		}
	} else {
		err := errors.New("Failed to fetch isvEnclaveQuoteBody from attestation report")
		return nil, err
	}
	return pubK, nil
}
