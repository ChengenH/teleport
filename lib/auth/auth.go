/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
// package auth implements certificate signing authority and access control server
// Authority server is composed of several parts:
//
// * Authority server itself that implements signing and acl logic
// * HTTP server wrapper for authority server
// * HTTP client wrapper
//
package auth

import (
	"fmt"

	"os"
	"time"

	"github.com/gravitational/configure/cstrings"
	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/backend/encryptedbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk/encryptor"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
)

// Authority implements minimal key-management facility for generating OpenSSH
//compatible public/private key pairs and OpenSSH certificates
type Authority interface {
	// GenerateKeyPair generates new keypair
	GenerateKeyPair(passphrase string) (privKey []byte, pubKey []byte, err error)

	// GetNewKeyPairFromPool returns new keypair from pre-generated in memory pool
	GetNewKeyPairFromPool() (privKey []byte, pubKey []byte, err error)

	// GenerateHostCert generates host certificate, it takes pkey as a signing
	// private key (host certificate authority)
	GenerateHostCert(pkey, key []byte, hostname, authDomain string, role teleport.Role, ttl time.Duration) ([]byte, error)

	// GenerateHostCert generates user certificate, it takes pkey as a signing
	// private key (user certificate authority)
	GenerateUserCert(pkey, key []byte, username string, ttl time.Duration) ([]byte, error)
}

// Session is a web session context, stores temporary key-value pair and session id
type Session struct {
	// ID is a session ID
	ID string `json:"id"`
	// User is a user this session belongs to
	User services.User `json:"user"`
	// WS is a private keypair used for signing requests
	WS services.WebSession `json:"web"`
}

// AuthServerOption allows setting options as functional arguments to AuthServer
type AuthServerOption func(*AuthServer)

// AuthClock allows setting clock for auth server (used in tests)
func AuthClock(clock clockwork.Clock) AuthServerOption {
	return func(a *AuthServer) {
		a.clock = clock
	}
}

// NewAuthServer returns a new AuthServer instance
func NewAuthServer(bk *encryptedbk.ReplicatedBackend, a Authority, hostname string, opts ...AuthServerOption) *AuthServer {
	as := AuthServer{}

	for _, o := range opts {
		o(&as)
	}

	if as.clock == nil {
		as.clock = clockwork.NewRealClock()
	}

	as.bk = bk
	as.Authority = a

	as.CAService = services.NewCAService(as.bk)
	as.LockService = services.NewLockService(as.bk)
	as.PresenceService = services.NewPresenceService(as.bk)
	as.ProvisioningService = services.NewProvisioningService(as.bk)
	as.WebService = services.NewWebService(as.bk)
	as.BkKeysService = services.NewBkKeysService(as.bk)

	as.Hostname = hostname
	return &as
}

// AuthServer implements key signing, generation and ACL functionality
// used by teleport
type AuthServer struct {
	clock clockwork.Clock
	bk    *encryptedbk.ReplicatedBackend
	Authority
	Hostname string

	*services.CAService
	*services.LockService
	*services.PresenceService
	*services.ProvisioningService
	*services.WebService
	*services.BkKeysService
}

// GetLocalDomain returns domain name that identifies this authority server
func (a *AuthServer) GetLocalDomain() (string, error) {
	return a.Hostname, nil
}

// GenerateHostCert generates host certificate, it takes pkey as a signing
// private key (host certificate authority)
func (s *AuthServer) GenerateHostCert(
	key []byte, hostname, authDomain string, role teleport.Role, ttl time.Duration) ([]byte, error) {

	ca, err := s.CAService.GetCertAuthority(services.CertAuthID{
		Type:       services.HostCA,
		DomainName: s.Hostname,
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	privateKey, err := ca.FirstSigningKey()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return s.Authority.GenerateHostCert(privateKey, key, hostname, authDomain, role, ttl)
}

// GenerateUserCert generates user certificate, it takes pkey as a signing
// private key (user certificate authority)
func (s *AuthServer) GenerateUserCert(
	key []byte, username string, ttl time.Duration) ([]byte, error) {

	ca, err := s.CAService.GetCertAuthority(services.CertAuthID{
		Type:       services.UserCA,
		DomainName: s.Hostname,
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	privateKey, err := ca.FirstSigningKey()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return s.Authority.GenerateUserCert(privateKey, key, username, ttl)
}

func (s *AuthServer) SignIn(user string, password []byte) (*Session, error) {
	if err := s.CheckPasswordWOToken(user, password); err != nil {
		return nil, trace.Wrap(err)
	}
	sess, err := s.NewWebSession(user)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := s.UpsertWebSession(user, sess, WebSessionTTL); err != nil {
		return nil, trace.Wrap(err)
	}
	sess.WS.Priv = nil
	return sess, nil
}

// CreateWebSession creates a new web session for a user based on a valid previous sessionID,
// method is used to renew the web session for a user
func (s *AuthServer) CreateWebSession(user string, prevSessionID string) (*Session, error) {
	_, err := s.GetWebSession(user, prevSessionID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess, err := s.NewWebSession(user)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := s.UpsertWebSession(user, sess, WebSessionTTL); err != nil {
		return nil, trace.Wrap(err)
	}
	sess.WS.Priv = nil
	return sess, nil
}

func (s *AuthServer) GenerateToken(nodeName string, role teleport.Role, ttl time.Duration) (string, error) {
	if !cstrings.IsValidDomainName(nodeName) {
		return "", trace.Wrap(teleport.BadParameter("nodeName",
			fmt.Sprintf("'%v' is not a valid dns name", nodeName)))
	}
	if err := role.Check(); err != nil {
		return "", trace.Wrap(err)
	}
	token, err := utils.CryptoRandomHex(TokenLenBytes)
	if err != nil {
		return "", trace.Wrap(err)
	}
	outputToken, err := services.JoinTokenRole(token, string(role))
	if err != nil {
		return "", err
	}
	if err := s.ProvisioningService.UpsertToken(token, nodeName, string(role), ttl); err != nil {
		return "", err
	}
	return outputToken, nil
}

func (s *AuthServer) ValidateToken(token, domainName string) (role string, e error) {
	token, _, err := services.SplitTokenRole(token)
	if err != nil {
		return "", trace.Wrap(err)
	}
	tok, err := s.ProvisioningService.GetToken(token)
	if err != nil {
		return "", trace.Wrap(err)
	}
	if tok.DomainName != domainName {
		return "", trace.Errorf("domainName does not match")
	}
	return tok.Role, nil
}

func (s *AuthServer) RegisterUsingToken(outputToken, nodename string, role teleport.Role) (keys PackedKeys, e error) {
	log.Infof("[AUTH] Node `%v` is trying to join", nodename)
	if err := role.Check(); err != nil {
		return PackedKeys{}, trace.Wrap(err)
	}
	token, _, err := services.SplitTokenRole(outputToken)
	if err != nil {
		return PackedKeys{}, trace.Wrap(err)
	}
	tok, err := s.ProvisioningService.GetToken(token)
	if err != nil {
		log.Warningf("[AUTH] Node `%v` cannot join: token error. %v", nodename, err)
		return PackedKeys{}, trace.Wrap(err)
	}
	if tok.DomainName != nodename {
		return PackedKeys{}, trace.Wrap(
			teleport.BadParameter("domainName", "domainName does not match"))
	}

	if tok.Role != string(role) {
		return PackedKeys{}, trace.Wrap(
			teleport.BadParameter("token.Role", "role does not match"))
	}
	k, pub, err := s.GenerateKeyPair("")
	if err != nil {
		return PackedKeys{}, trace.Wrap(err)
	}
	// we always append authority's domain to resulting node name,
	// that's how we make sure that nodes are uniquely identified/found
	// in cases when we have multiple environments/organizations
	fqdn := fmt.Sprintf("%s.%s", nodename, s.Hostname)
	c, err := s.GenerateHostCert(pub, fqdn, s.Hostname, role, 0)
	if err != nil {
		log.Warningf("[AUTH] Node `%v` cannot join: cert generation error. %v", nodename, err)
		return PackedKeys{}, trace.Wrap(err)
	}

	keys = PackedKeys{
		Key:  k,
		Cert: c,
	}

	if err := s.DeleteToken(outputToken); err != nil {
		return PackedKeys{}, trace.Wrap(err)
	}

	utils.Consolef(os.Stdout, "[AUTH] Node `%v` joined the cluster", nodename)
	return keys, nil
}

func (s *AuthServer) RegisterNewAuthServer(domainName, outputToken string,
	publicSealKey encryptor.Key) (masterKey encryptor.Key, e error) {

	token, _, err := services.SplitTokenRole(outputToken)
	if err != nil {
		return encryptor.Key{}, trace.Wrap(err)
	}

	tok, err := s.ProvisioningService.GetToken(token)
	if err != nil {
		return encryptor.Key{}, trace.Wrap(err)
	}
	if tok.DomainName != domainName {
		return encryptor.Key{}, trace.Wrap(
			teleport.AccessDenied("domainName does not match"))
	}

	if tok.Role != string(teleport.RoleAuth) {
		return encryptor.Key{}, trace.Wrap(
			teleport.AccessDenied("role does not match"))
	}

	if err := s.DeleteToken(outputToken); err != nil {
		return encryptor.Key{}, trace.Wrap(err)
	}

	if err := s.BkKeysService.AddSealKey(publicSealKey); err != nil {
		return encryptor.Key{}, trace.Wrap(err)
	}

	localKey, err := s.BkKeysService.GetSignKey()
	if err != nil {
		return encryptor.Key{}, trace.Wrap(err)
	}

	return localKey.Public(), nil
}

func (s *AuthServer) DeleteToken(outputToken string) error {
	token, _, err := services.SplitTokenRole(outputToken)
	if err != nil {
		return err
	}
	return s.ProvisioningService.DeleteToken(token)
}

func (s *AuthServer) NewWebSession(userName string) (*Session, error) {
	token, err := utils.CryptoRandomHex(WebSessionTokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	bearerToken, err := utils.CryptoRandomHex(WebSessionTokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	priv, pub, err := s.GetNewKeyPairFromPool()
	if err != nil {
		return nil, err
	}
	ca, err := s.CAService.GetCertAuthority(services.CertAuthID{
		Type:       services.UserCA,
		DomainName: s.Hostname,
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	privateKey, err := ca.FirstSigningKey()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cert, err := s.Authority.GenerateUserCert(privateKey, pub, userName, WebSessionTTL)
	if err != nil {
		return nil, err
	}
	user, err := s.GetUser(userName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess := &Session{
		ID:   token,
		User: *user,
		WS: services.WebSession{
			Priv:        priv,
			Pub:         cert,
			Expires:     s.clock.Now().UTC().Add(WebSessionTTL),
			BearerToken: bearerToken,
		},
	}
	return sess, nil
}

func (s *AuthServer) UpsertWebSession(user string, sess *Session, ttl time.Duration) error {
	return s.WebService.UpsertWebSession(user, sess.ID, sess.WS, ttl)
}

func (s *AuthServer) GetWebSession(userName string, id string) (*Session, error) {
	ws, err := s.WebService.GetWebSession(userName, id)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	user, err := s.GetUser(userName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &Session{
		ID:   id,
		User: *user,
		WS:   *ws,
	}, nil
}

func (s *AuthServer) GetWebSessionInfo(userName string, id string) (*Session, error) {
	sess, err := s.WebService.GetWebSession(userName, id)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	user, err := s.GetUser(userName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess.Priv = nil
	return &Session{
		ID:   id,
		User: *user,
		WS:   *sess,
	}, nil
}

func (s *AuthServer) DeleteWebSession(user string, id string) error {
	return trace.Wrap(s.WebService.DeleteWebSession(user, id))
}

const (
	// WebSessionTTL specifies standard web session time to live
	WebSessionTTL = 10 * time.Minute
	// TokenLenBytes is len in bytes of the invite token
	TokenLenBytes = 16
	// WebSessionTokenLenBytes specifies len in bytes of the
	// web session random token
	WebSessionTokenLenBytes = 32
)
