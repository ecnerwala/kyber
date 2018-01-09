// Package dss implements the Distributed Schnorr Signature protocol from the
// paper "Provably Secure Distributed Schnorr Signatures and a (t, n)
// Threshold Scheme for Implicit Certificates".
// https://dl.acm.org/citation.cfm?id=678297
// To generate a distributed signature from a group of participants, the group
// must first generate one longterm distributed secret with the share/dkg
// package, and then one random secret to be used only once.
// Each participant then creates a DSS struct, that can issue partial signatures
// with `dss.PartialSignature()`. These partial signatures can be broadcasted to
// the whole group or to a trusted combiner. Once one has collected enough
// partial signatures, it is possible to compute the distributed signature with
// the `Signature` method.
// The resulting signature is compatible with the EdDSA verification function.
// against the longterm distributed key.
package dss

import (
	"bytes"
	"crypto/sha512"
	"errors"
	"fmt"

	"gopkg.in/dedis/kyber.v1"
	"gopkg.in/dedis/kyber.v1/share"
	"gopkg.in/dedis/kyber.v1/sign/eddsa"
	"gopkg.in/dedis/kyber.v1/sign/schnorr"
)

// Suite represents the functionalities needed by the dss package
type Suite interface {
	kyber.Group
	kyber.HashFactory
	kyber.Random
}

// DistKeyShare is an abstraction to allow one to use distributed key share
// from different schemes easily into this distributed threshold Schnorr
// signature framework.
type DistKeyShare interface {
	PriShare() *share.PriShare
	Commitments() []kyber.Point
}

// DSS holds the information used to issue partial signatures as well as to
// compute the distributed schnorr signature.
type DSS struct {
	suite        Suite
	secret       kyber.Scalar
	public       kyber.Point
	index        int
	participants []kyber.Point
	T            int
	long         DistKeyShare
	random       DistKeyShare
	longPoly     *share.PubPoly
	randomPoly   *share.PubPoly
	msg          []byte
	partials     []*share.PriShare
	partialsIdx  map[int]bool
	signed       bool
	sessionID    []byte
}

// PartialSig is partial representation of the final distributed signature. It
// must be sent to each of the other participants.
type PartialSig struct {
	Partial   *share.PriShare
	SessionID []byte
	Signature []byte
}

// NewDSS returns a DSS struct out of the suite, the longterm secret of this
// node, the list of participants, the longterm and random distributed key
// (generated by the dkg package), the message to sign and finally the T
// threshold. It returns an error if the public key of the secret can't be found
// in the list of participants.
func NewDSS(suite Suite, secret kyber.Scalar, participants []kyber.Point,
	long, random DistKeyShare, msg []byte, T int) (*DSS, error) {
	public := suite.Point().Mul(secret, nil)
	var i int
	var found bool
	for j, p := range participants {
		if p.Equal(public) {
			found = true
			i = j
			break
		}
	}
	if !found {
		return nil, errors.New("dss: public key not found in list of participants")
	}
	return &DSS{
		suite:        suite,
		secret:       secret,
		public:       public,
		index:        i,
		participants: participants,
		long:         long,
		longPoly:     share.NewPubPoly(suite, suite.Point().Base(), long.Commitments()),
		random:       random,
		randomPoly:   share.NewPubPoly(suite, suite.Point().Base(), random.Commitments()),
		msg:          msg,
		T:            T,
		partialsIdx:  make(map[int]bool),
		sessionID:    sessionID(suite, long, random),
	}, nil
}

// PartialSig generates the partial signature related to this DSS. This
// PartialSig can be broadcasted to every other participant or only to a
// trusted combiner as described in the paper.
// The signature format is compatible with EdDSA verification implementations.
func (d *DSS) PartialSig() (*PartialSig, error) {
	// following the notations from the paper
	alpha := d.long.PriShare().V
	beta := d.random.PriShare().V
	hash := d.hashSig()
	right := d.suite.Scalar().Mul(hash, alpha)
	ps := &PartialSig{
		Partial: &share.PriShare{
			V: right.Add(right, beta),
			I: d.index,
		},
		SessionID: d.sessionID,
	}
	var err error
	ps.Signature, err = schnorr.Sign(d.suite, d.secret, ps.Hash(d.suite))
	if !d.signed {
		d.partialsIdx[d.index] = true
		d.partials = append(d.partials, ps.Partial)
		d.signed = true
	}
	return ps, err
}

// ProcessPartialSig takes a PartialSig from another participant and stores it
// for generating the distributed signature. It returns an error if the index is
// wrong, or the signature is invalid or if a partial signature has already been
// received by the same peer. To know whether the distributed signature can be
// computed after this call, one can use the `EnoughPartialSigs` method.
func (d *DSS) ProcessPartialSig(ps *PartialSig) error {
	public, ok := findPub(d.participants, ps.Partial.I)
	if !ok {
		return errors.New("dss: partial signature with invalid index")
	}

	if err := schnorr.Verify(d.suite, public, ps.Hash(d.suite), ps.Signature); err != nil {
		return err
	}

	// nothing secret here
	if !bytes.Equal(ps.SessionID, d.sessionID) {
		return errors.New("dss: session id do not match")
	}

	if _, ok := d.partialsIdx[ps.Partial.I]; ok {
		return errors.New("dss: partial signature already received from peer")
	}

	hash := d.hashSig()
	idx := ps.Partial.I
	randShare := d.randomPoly.Eval(idx)
	longShare := d.longPoly.Eval(idx)
	right := d.suite.Point().Mul(hash, longShare.V)
	right.Add(randShare.V, right)
	left := d.suite.Point().Mul(ps.Partial.V, nil)
	if !left.Equal(right) {
		return errors.New("dss: partial signature not valid")
	}
	d.partialsIdx[ps.Partial.I] = true
	d.partials = append(d.partials, ps.Partial)
	return nil
}

// EnoughPartialSig returns true if there are enough partial signature to compute
// the distributed signature. It returns false otherwise. If there are enough
// partial signatures, one can issue the signature with `Signature()`.
func (d *DSS) EnoughPartialSig() bool {
	return len(d.partials) >= d.T
}

// Signature computes the distributed signature from the list of partial
// signatures received. It returns an error if there are not enough partial
// signatures. The signature is compatible with the EdDSA verification
// alrogithm.
func (d *DSS) Signature() ([]byte, error) {
	if !d.EnoughPartialSig() {
		return nil, errors.New("dkg: not enough partial signatures to sign")
	}
	gamma, err := share.RecoverSecret(d.suite, d.partials, d.T, len(d.participants))
	if err != nil {
		fmt.Println("or here")
		return nil, err
	}
	// RandomPublic || gamma
	var buff bytes.Buffer
	_, _ = d.random.Commitments()[0].MarshalTo(&buff)
	_, _ = gamma.MarshalTo(&buff)
	return buff.Bytes(), nil
}

func (d *DSS) hashSig() kyber.Scalar {
	// H(R || A || msg) with
	//  * R = distributed random "key"
	//  * A = distributed public key
	//  * msg = msg to sign
	h := sha512.New()
	_, _ = d.random.Commitments()[0].MarshalTo(h)
	_, _ = d.long.Commitments()[0].MarshalTo(h)
	_, _ = h.Write(d.msg)
	return d.suite.Scalar().SetBytes(h.Sum(nil))
}

// Verify takes a public key, a message and a signature and returns an error if
// the signature is invalid.
func Verify(public kyber.Point, msg, sig []byte) error {
	return eddsa.Verify(public, msg, sig)
}

// Hash returns the hash representation of this PartialSig to be used in a
// signature.
func (ps *PartialSig) Hash(s Suite) []byte {
	h := s.Hash()
	_, _ = h.Write(ps.Partial.Hash(s))
	_, _ = h.Write(ps.SessionID)
	return h.Sum(nil)
}

func findPub(list []kyber.Point, i int) (kyber.Point, bool) {
	if i >= len(list) {
		return nil, false
	}
	return list[i], true
}

func sessionID(s Suite, a, b DistKeyShare) []byte {
	h := s.Hash()
	for _, p := range a.Commitments() {
		_, _ = p.MarshalTo(h)
	}

	for _, p := range b.Commitments() {
		_, _ = p.MarshalTo(h)
	}

	return h.Sum(nil)
}
