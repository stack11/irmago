package irmago

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"

	"math/big"

	"github.com/mhe/gabi"
)

type KeyshareSessionHandler interface {
	AskPin(remainingAttempts int, callback func(pin string))
	KeyshareDone(message interface{})
	KeyshareBlocked(duration int)
	KeyshareError(err error)
}

type keyshareSession struct {
	session        Session
	builders       []gabi.ProofBuilder
	transports     map[SchemeManagerIdentifier]*HTTPTransport
	sessionHandler KeyshareSessionHandler
	keyshareServer *keyshareServer
}

type keyshareServer struct {
	URL        string              `json:"url"`
	Username   string              `json:"username"`
	Nonce      []byte              `json:"nonce"`
	PrivateKey *paillierPrivateKey `json:"keyPair"`
	token      string
}

type keyshareRegistration struct {
	Username  string             `json:"username"`
	Pin       string             `json:"pin"`
	PublicKey *paillierPublicKey `json:"publicKey"`
}

type keyshareAuthorization struct {
	Status     string   `json:"status"`
	Candidates []string `json:"candidates"`
}

type keysharePinMessage struct {
	Username string `json:"id"`
	Pin      string `json:"pin"`
}

type keysharePinStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type publicKeyIdentifier struct {
	Issuer  string `json:"issuer"`
	Counter uint   `json:"counter"`
}

// TODO update protocol so this can go away
func (pki *publicKeyIdentifier) MarshalJSON() ([]byte, error) {
	temp := struct {
		Issuer  map[string]string `json:"issuer"`
		Counter uint              `json:"counter"`
	}{
		Issuer:  map[string]string{"identifier": pki.Issuer},
		Counter: pki.Counter,
	}
	return json.Marshal(temp)
}

type proofPCommitmentMap struct {
	Commitments map[publicKeyIdentifier]*gabi.ProofPCommitment `json:"c"`
}

type KeyshareHandler interface {
	StartKeyshareRegistration(manager *SchemeManager, registrationCallback func(email, pin string))
}

const (
	kssUsernameHeader = "IRMA_Username"
	kssAuthHeader     = "IRMA_Authorization"
	kssAuthorized     = "authorized"
	kssTokenExpired   = "expired"
	kssPinSuccess     = "success"
	kssPinFailure     = "failure"
	kssPinError       = "error"
)

func newKeyshareServer(privatekey *paillierPrivateKey, url, email string) (ks *keyshareServer, err error) {
	ks = &keyshareServer{
		Nonce:      make([]byte, 32),
		URL:        url,
		Username:   email,
		PrivateKey: privatekey,
	}
	_, err = rand.Read(ks.Nonce)
	return
}

func (ks *keyshareServer) HashedPin(pin string) string {
	hash := sha256.Sum256(append(ks.Nonce, []byte(pin)...))
	return base64.RawStdEncoding.EncodeToString(hash[:])
}

func StartKeyshareSession(
	session Session,
	builders []gabi.ProofBuilder,
	sessionHandler KeyshareSessionHandler,
) {
	ksscount := 0
	for _, managerId := range session.SchemeManagers() {
		if MetaStore.SchemeManagers[managerId].Distributed() {
			ksscount++
			if _, registered := Manager.keyshareServers[managerId]; !registered {
				err := errors.New("Not registered to keyshare server of scheme manager " + managerId.String())
				sessionHandler.KeyshareError(err)
				return
			}
		}
	}
	if _, issuing := session.(*IssuanceRequest); issuing && ksscount > 1 {
		err := errors.New("Issuance session involving more than one keyshare servers are not supported")
		sessionHandler.KeyshareError(err)
		return
	}

	ks := &keyshareSession{
		session:        session,
		builders:       builders,
		sessionHandler: sessionHandler,
		transports:     map[SchemeManagerIdentifier]*HTTPTransport{},
	}

	askPin := false

	for _, managerId := range session.SchemeManagers() {
		if !MetaStore.SchemeManagers[managerId].Distributed() {
			continue
		}

		ks.keyshareServer = Manager.keyshareServers[managerId]
		transport := NewHTTPTransport(ks.keyshareServer.URL)
		transport.SetHeader(kssUsernameHeader, ks.keyshareServer.Username)
		transport.SetHeader(kssAuthHeader, ks.keyshareServer.token)
		ks.transports[managerId] = transport

		authstatus := &keyshareAuthorization{}
		err := transport.Post("users/isAuthorized", authstatus, "")
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
			return
		}
		switch authstatus.Status {
		case kssAuthorized: // nop
		case kssTokenExpired:
			askPin = true
		default:
			ks.sessionHandler.KeyshareError(errors.New("Keyshare server returned unrecognized authirization status"))
			return
		}
	}

	if askPin {
		ks.VerifyPin(-1)
	}
}

// Ask for a pin, repeatedly if necessary, and either continue the keyshare protocol
// with authorization, or stop the keyshare protocol and inform of failure.
func (ks *keyshareSession) VerifyPin(attempts int) {
	ks.sessionHandler.AskPin(attempts, func(pin string) {
		success, attemptsRemaining, blocked, err := ks.verifyPinAttempt(pin)
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
			return
		}
		if blocked != 0 {
			ks.sessionHandler.KeyshareBlocked(blocked)
			return
		}
		if success {
			ks.GetCommitments()
			return
		}
		// Not successful but no error and not yet blocked: try again
		ks.VerifyPin(attemptsRemaining)
	})
}

// Verify the specified pin at each of the keyshare servers involved in the specified session.
//
// - If the pin did not verify at one of the keyshare servers but there are attempts remaining,
// the amount of remaining attempts is returned as the second return value.
//
// - If the pin did not verify at one of the keyshare servers and there are no attempts remaining,
// the amount of time for which we are blocked at the keyshare server is returned as the third
// parameter.
//
// - If this or anything else (specified in err) goes wrong, success will be false.
// If all is ok, success will be true.
func (ks *keyshareSession) verifyPinAttempt(pin string) (success bool, tries int, blocked int, err error) {
	for _, managerId := range ks.session.SchemeManagers() {
		if !MetaStore.SchemeManagers[managerId].Distributed() {
			continue
		}

		kss := Manager.keyshareServers[managerId]
		transport := ks.transports[managerId]
		pinmsg := keysharePinMessage{Username: kss.Username, Pin: kss.HashedPin(pin)}
		pinresult := &keysharePinStatus{}
		err = transport.Post("users/verify/pin", pinresult, pinmsg)
		if err != nil {
			return
		}

		switch pinresult.Status {
		case kssPinSuccess:
			kss.token = pinresult.Message
			transport.SetHeader(kssAuthHeader, kss.token)
		case kssPinFailure:
			tries, err = strconv.Atoi(pinresult.Message)
			if err != nil {
				return
			}
			return
		case kssPinError:
			blocked, err = strconv.Atoi(pinresult.Message)
			if err != nil {
				return
			}
			return
		default:
			err = errors.New("Keyshare server returned unrecognized PIN status")
			return
		}
	}

	success = true
	return
}

// GetCommitments gets the commitments (first message in Schnorr zero-knowledge protocol)
// of all keyshare servers of their part of the private key, and merges these commitments
// in our own proof builders.
func (ks *keyshareSession) GetCommitments() {
	pkids := map[SchemeManagerIdentifier][]*publicKeyIdentifier{}
	commitments := map[publicKeyIdentifier]*gabi.ProofPCommitment{}

	// For each scheme manager, build a list of public keys under this manager
	// that we will use in the keyshare protocol with the keyshare server of this manager
	for _, builder := range ks.builders {
		pk := builder.PublicKey()
		managerId := NewIssuerIdentifier(pk.Issuer).SchemeManagerIdentifier()
		if !MetaStore.SchemeManagers[managerId].Distributed() {
			continue
		}
		if _, contains := pkids[managerId]; !contains {
			pkids[managerId] = []*publicKeyIdentifier{}
		}
		pkids[managerId] = append(pkids[managerId], &publicKeyIdentifier{Issuer: pk.Issuer, Counter: pk.Counter})
	}

	// Now inform each keyshare server of with respect to which public keys
	// we want them to send us commitments
	for _, managerId := range ks.session.SchemeManagers() {
		if !MetaStore.SchemeManagers[managerId].Distributed() {
			continue
		}

		transport := ks.transports[managerId]
		comms := &proofPCommitmentMap{}
		err := transport.Post("prove/getCommitments", comms, pkids[managerId])
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
			return
		}
		for pki, c := range comms.Commitments {
			commitments[pki] = c
		}
	}

	// Merge in the commitments
	for _, builder := range ks.builders {
		pk := builder.PublicKey()
		pki := publicKeyIdentifier{Issuer: pk.Issuer, Counter: pk.Counter}
		comm, distributed := commitments[pki]
		if !distributed {
			continue
		}
		builder.MergeProofPCommitment(comm)
	}

	ks.GetProofPs()
}

// GetProofPs uses the combined commitments of all keyshare servers and ourself
// to calculate the challenge, which is sent to the keyshare servers in order to
// receive their responses (2nd and 3rd message in Schnorr zero-knowledge protocol).
func (ks *keyshareSession) GetProofPs() {
	_, issig := ks.session.(*SignatureRequest)
	_, issuing := ks.session.(*IssuanceRequest)
	challenge := gabi.DistributedChallenge(ks.session.GetContext(), ks.session.GetNonce(), ks.builders, issig)
	kssChallenge := challenge

	// In disclosure or signature sessions the challenge is Paillier encrypted.
	if !issuing {
		bytes, err := ks.keyshareServer.PrivateKey.Encrypt(challenge.Bytes())
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
		}
		kssChallenge = new(big.Int).SetBytes(bytes)
	}

	// Post the challenge, obtaining JWT's containing the ProofP's
	responses := map[SchemeManagerIdentifier]string{}
	for _, managerId := range ks.session.SchemeManagers() {
		transport, distributed := ks.transports[managerId]
		if !distributed {
			continue
		}
		var jwt string
		err := transport.Post("prove/getResponse", &jwt, kssChallenge)
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
			return
		}
		responses[managerId] = jwt
	}

	ks.Finish(challenge, responses)
}

// Finish the keyshare protocol: in case of issuance, put the keyshare jwt in the
// IssueCommitmentMessage; in case of disclosure and signing, parse each keyshare jwt,
// merge in the received ProofP's, and finish.
func (ks *keyshareSession) Finish(challenge *big.Int, responses map[SchemeManagerIdentifier]string) {
	switch ks.session.(type) {
	case *DisclosureRequest:
	case *SignatureRequest:
		proofPs := make([]*gabi.ProofP, len(ks.builders))
		for i, builder := range ks.builders {
			// Parse each received JWT
			managerId := NewIssuerIdentifier(builder.PublicKey().Issuer).SchemeManagerIdentifier()
			if !MetaStore.SchemeManagers[managerId].Distributed() {
				continue
			}
			msg := struct {
				ProofP *gabi.ProofP
			}{}
			_, err := JwtDecode(responses[managerId], msg)
			if err != nil {
				ks.sessionHandler.KeyshareError(err)
				return
			}

			// Decrypt the responses and populate a slice of ProofP's
			proofPs[i] = msg.ProofP
			bytes, err := ks.keyshareServer.PrivateKey.Decrypt(proofPs[i].C.Bytes())
			if err != nil {
				ks.sessionHandler.KeyshareError(err)
				return
			}
			proofPs[i].C = new(big.Int).SetBytes(bytes)
		}

		// Create merged proofs and finish protocol
		list, err := gabi.BuildDistributedProofList(challenge, ks.builders, proofPs)
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
			return
		}
		ks.sessionHandler.KeyshareDone(list)

	case *IssuanceRequest:
		// Calculate IssueCommitmentMessage, without merging in any of the received ProofP's:
		// instead, include the keyshare server's JWT in the IssueCommitmentMessage for the
		// issuance server to verify
		message, err := Manager.IssueCommitments(ks.session.(*IssuanceRequest))
		if err != nil {
			ks.sessionHandler.KeyshareError(err)
			return
		}
		for _, response := range responses {
			message.ProofPjwt = response
			break
		}
		ks.sessionHandler.KeyshareDone(message)
	}
}

// TODO this message is ugly, should update protocol
func (comms *proofPCommitmentMap) UnmarshalJSON(bytes []byte) error {
	comms.Commitments = map[publicKeyIdentifier]*gabi.ProofPCommitment{}
	temp := struct {
		C [][]*json.RawMessage `json:"c"`
	}{}
	if err := json.Unmarshal(bytes, &temp); err != nil {
		return err
	}
	for _, raw := range temp.C {
		tempPkId := struct {
			Issuer struct {
				Identifier string `json:"identifier"`
			} `json:"issuer"`
			Counter uint `json:"counter"`
		}{}
		comm := gabi.ProofPCommitment{}
		if err := json.Unmarshal([]byte(*raw[0]), &tempPkId); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(*raw[1]), &comm); err != nil {
			return err
		}
		pkid := publicKeyIdentifier{Issuer: tempPkId.Issuer.Identifier, Counter: tempPkId.Counter}
		comms.Commitments[pkid] = &comm
	}
	return nil
}