package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

// Common errors for credential manager
var (
	ErrMissingProfileDefault = fmt.Errorf("missing profile: default")
	errMFANeeded             = fmt.Errorf("MFA needed")
	errBaseMFANeeded         = fmt.Errorf("Base MFA needed")
	errUnknownProfile        = fmt.Errorf("Unknown profile")
	errSourceSessionExpired  = fmt.Errorf("Source session expired")
)

type fatalError struct {
	err error
}

func (e fatalError) Error() string {
	return fmt.Sprintf("%v", e.err.Error())
}

func makeFatal(err error) error {
	return &fatalError{
		err: err,
	}
}

func isFatalError(err error) bool {
	_, ok := err.(*fatalError)
	return ok
}

// CredentialsManager provides an interface
type CredentialsManager interface {
	Role() string
	RetrieveRole(name, MFA string) (*sts.Credentials, error)
	RetrieveRoleARN(RoleARN, MFASerial, MFA string) (*sts.Credentials, error)
	AssumeRole(name, mfa string) error
	AssumeRoleARN(name, RoleARN, MFASerial, MFA string) error
	GetCredentials() (*sts.Credentials, error)
	SetSourceProfile(name, mfa string) error
}

// CredentialsExpirationManager is responsible for renewing a set of credentials
type CredentialsExpirationManager struct {
	lock sync.Mutex

	// config is the loaded configuration
	config Config

	// profile is the current base profile
	sourceProfile     Profile
	sourceProfileName string

	// err is the current internal error
	err error

	// This is the default session and information
	sourceSession     *session.Session
	sourceCredentials *sts.Credentials
	sourceSTSClient   *sts.STS

	// This is the current active credentials
	role        string
	credentials *sts.Credentials
}

// NewCredentialsExpirationManager returns a credentialsExpirationManager
// It creates a session, then it will call GetSessionToken to retrieve a pair of
// temporary credentials.
func NewCredentialsExpirationManager(profileName string, conf Config, mfa string) *CredentialsExpirationManager {
	cm := &CredentialsExpirationManager{
		role:   profileName,
		config: conf,
	}
	err := cm.SetSourceProfile(profileName, mfa)
	if err != nil {
		fmt.Fprintf(errout, "%v\n", err)
		if isFatalError(err) {
			os.Exit(1)
		}
	}

	go cm.Refresher()
	return cm
}

// SetSourceProfile updates the credentials manager with new soruce profile.
// This operation will also update the current profile to the source profile
func (m *CredentialsExpirationManager) SetSourceProfile(name, mfa string) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	fatal := false
	checkErr := func(err error) error {
		if fatal {
			return makeFatal(err)
		}
		return err
	}
	m.err = nil

	log.Printf("Setting base profile: %v", name)
	profile, ok := m.config.profiles[name]
	if !ok {
		m.err = errUnknownProfile
		if name != "default" {
			return makeFatal(errUnknownProfile)
		}
		return errUnknownProfile
	}

	sess := session.New(&aws.Config{
		Region: &profile.Region,
		Credentials: credentials.NewStaticCredentials(
			profile.AwsAccessKeyID,
			profile.AwsSecretAccessKey,
			profile.AwsSessionToken,
		),
	})
	stsClient := sts.New(sess)

	if profile.MFASerial != "" && mfa == "" {
		m.err = errMFANeeded
		return errMFANeeded
	}

	sessionTokenInput := &sts.GetSessionTokenInput{
		DurationSeconds: aws.Int64(10 * 3600),
	}

	if profile.MFASerial != "" {
		sessionTokenInput.SerialNumber = aws.String(profile.MFASerial)
	}
	if mfa != "" {
		sessionTokenInput.TokenCode = aws.String(mfa)
		fatal = true
	}

	sessionTokenResp, err := stsClient.GetSessionToken(sessionTokenInput)
	if err != nil {
		err = checkErr(err)
		m.err = err
		return err
	}

	m.credentials = sessionTokenResp.Credentials
	m.sourceCredentials = sessionTokenResp.Credentials
	m.sourceSession = session.New(&aws.Config{
		Region: &profile.Region,
		Credentials: credentials.NewStaticCredentials(
			*m.credentials.AccessKeyId,
			*m.credentials.SecretAccessKey,
			*m.credentials.SessionToken,
		),
	})
	m.role = name
	m.sourceProfile = profile
	m.sourceProfileName = name
	m.sourceSTSClient = sts.New(m.sourceSession)
	return nil
}

// Role returns the name of the current active role
func (m *CredentialsExpirationManager) Role() string {
	return m.role
}

// Refresher starts a Go routine and refreshes the credentials
func (m *CredentialsExpirationManager) Refresher() {
	for {
		select {
		case <-time.After(10 * time.Second):
			if m.err != nil {
				continue
			}
			m.refreshCredentials()
		}
	}
}

// AssumeRole changes (assumes) the role `name`. An optional MFA can be passed
// to the function, if set to "" the MFA is ignored
func (m *CredentialsExpirationManager) AssumeRole(name, MFA string) error {
	profile, ok := m.config.profiles[name]
	if !ok {
		return errUnknownProfile
	}

	if profile.SourceProfile != m.sourceProfileName || m.sourceCredentialsExpired() {
		err := m.SetSourceProfile(profile.SourceProfile, MFA)
		if err != nil {
			return err
		}
	}

	fmt.Println("Assuming: ", name)
	return m.AssumeRoleARN(name, profile.RoleARN, profile.MFASerial, MFA)
}

// RetrieveRole will assume and fetch temporary credentials, but does not update
// the role and credentials stored by the manager.
func (m *CredentialsExpirationManager) RetrieveRole(name, MFA string) (*sts.Credentials, error) {
	if m.err != nil {
		return nil, m.err
	}

	profile, ok := m.config.profiles[name]
	if !ok {
		return nil, errUnknownProfile
	}

	if profile.SourceProfile != m.sourceProfileName || m.sourceCredentialsExpired() {
		err := m.SetSourceProfile(profile.SourceProfile, MFA)
		if err != nil {
			return nil, err
		}
	}

	return m.RetrieveRoleARN(profile.RoleARN, profile.MFASerial, MFA)
}

// RetrieveRoleARN assumes and fetch temporary credentials based on the RoleArn
func (m *CredentialsExpirationManager) RetrieveRoleARN(RoleARN, MFASerial, MFA string) (*sts.Credentials, error) {
	if m.err != nil {
		return nil, m.err
	}

	if m.sourceCredentialsExpired() {
		err := m.SetSourceProfile(m.sourceProfileName, MFA)
		if err != nil {
			return nil, err
		}
	}

	// source profile is requested return sourceCredentials
	if RoleARN == m.sourceProfile.RoleARN {
		return m.sourceCredentials, nil
	}

	if MFASerial != "" && MFA == "" {
		return nil, errMFANeeded
	}

	assumeRoleInput := &sts.AssumeRoleInput{
		RoleArn:         &RoleARN,
		RoleSessionName: &m.sourceProfile.RoleSessionName,
	}

	if MFASerial != "" {
		assumeRoleInput.SerialNumber = &MFASerial
	}

	if MFA != "" {
		assumeRoleInput.TokenCode = &MFA
	}

	resp, err := m.sourceSTSClient.AssumeRole(assumeRoleInput)
	if err != nil {
		return nil, err
	}

	return resp.Credentials, nil
}

// AssumeRoleARN assumes the role specified by RoleARN and will store it as
// with the name specified.
func (m *CredentialsExpirationManager) AssumeRoleARN(name, RoleARN, MFASerial, MFA string) error {
	if m.err != nil {
		return m.err
	}

	creds, err := m.RetrieveRoleARN(RoleARN, MFASerial, MFA)
	if err != nil {
		return err
	}

	m.setCredentials(creds, name)
	return nil
}

// SetCredentials updates the stored credentials and the name of the role associated
// with the credentials
func (m *CredentialsExpirationManager) setCredentials(newCreds *sts.Credentials, role string) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.credentials = newCreds
	m.role = role
}

// GetCredentials returns the current saved credentials. The returned credentials
// are copied before they are returned.
func (m *CredentialsExpirationManager) GetCredentials() (*sts.Credentials, error) {
	if m.err != nil {
		return nil, m.err
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	return &sts.Credentials{
		AccessKeyId:     aws.String(*m.credentials.AccessKeyId),
		Expiration:      aws.Time(*m.credentials.Expiration),
		SecretAccessKey: aws.String(*m.credentials.SecretAccessKey),
		SessionToken:    aws.String(*m.credentials.SessionToken),
	}, nil
}

func (m *CredentialsExpirationManager) sourceCredentialsExpired() bool {
	return m.sourceSTSClient.Config.Credentials.IsExpired()
}

func (m *CredentialsExpirationManager) refreshCredentials() error {
	if m.sourceSTSClient == nil {
		return errors.New("No STS client set for refreshing credentials")
	}

	creds, err := m.GetCredentials()
	if err != nil {
		return err
	}

	if time.Now().Add(600 * time.Second).Before(*creds.Expiration) {
		// We no not need to refresh
		return nil
	}

	if m.role == "" || m.role == "default" {
		// Do not refresh main default role, let it time out
		return nil
	}

	fmt.Println("====> refreshing credentials")
	return m.AssumeRole(m.role, "")
}
