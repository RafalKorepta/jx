package credentials

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"

	"github.com/jenkins-x/jx/pkg/auth"
	"github.com/jenkins-x/jx/pkg/cmd/opts/step"
	"github.com/pkg/errors"

	"github.com/jenkins-x/jx/pkg/cmd/helper"

	"github.com/jenkins-x/jx/pkg/cmd/opts"
	"github.com/jenkins-x/jx/pkg/cmd/templates"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/spf13/cobra"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	optionOutputFile     = "output"
	optionGitHubAppOwner = "github-app-owner"
)

// StepGitCredentialsOptions contains the command line flags
type StepGitCredentialsOptions struct {
	step.StepOptions

	OutputFile        string
	GitHubAppOwner    string
	GitKind           string
	CredentialsSecret string
}

type credentials struct {
	user       string
	password   string
	serviceURL string
}

var (
	StepGitCredentialsLong = templates.LongDesc(`
		This pipeline step generates a Git credentials file for the current Git provider secrets

`)

	StepGitCredentialsExample = templates.Examples(`
		# generate the Git credentials file in the canonical location
		jx step git credentials

		# generate the Git credentials to a output file
		jx step git credentials -o /tmp/mycreds
`)
)

func NewCmdStepGitCredentials(commonOpts *opts.CommonOptions) *cobra.Command {
	options := StepGitCredentialsOptions{
		StepOptions: step.StepOptions{
			CommonOptions: commonOpts,
		},
	}
	cmd := &cobra.Command{
		Use:     "credentials",
		Short:   "Creates the Git credentials file for the current pipeline",
		Long:    StepGitCredentialsLong,
		Example: StepGitCredentialsExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&options.OutputFile, optionOutputFile, "o", "", "The output file name")
	cmd.Flags().StringVarP(&options.GitHubAppOwner, optionGitHubAppOwner, "g", "", "The owner (organisation or user name) if using GitHub App based tokens")
	cmd.Flags().StringVarP(&options.CredentialsSecret, "credentials-secret", "s", "", "The secret name to read the credentials from")
	cmd.Flags().StringVarP(&options.GitKind, "git-kind", "", "", "The git kind. e.g. github, bitbucketserver etc")
	return cmd
}

func (o *StepGitCredentialsOptions) Run() error {
	if os.Getenv("JX_CREDENTIALS_FROM_SECRET") != "" {
		log.Logger().Infof("Overriding CredentialsSecret from env var JX_CREDENTIALS_FROM_SECRET")
		o.CredentialsSecret = os.Getenv("JX_CREDENTIALS_FROM_SECRET")
	}

	outFile, err := o.determineOutputFile()
	if err != nil {
		return err
	}

	if o.CredentialsSecret != "" {
		// get secret
		kubeClient, ns, err := o.KubeClientAndDevNamespace()
		if err != nil {
			return err
		}

		secret, err := kubeClient.CoreV1().Secrets(ns).Get(o.CredentialsSecret, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "failed to find secret '%s' in namespace '%s'", o.CredentialsSecret, ns)
		}

		creds := credentials{
			user:       string(secret.Data["user"]),
			password:   string(secret.Data["token"]),
			serviceURL: string(secret.Data["url"]),
		}

		return o.createGitCredentialsFile(outFile, []credentials{creds})
	}

	gha, err := o.IsGitHubAppMode()
	if err != nil {
		return err
	}

	if gha && o.GitHubAppOwner == "" {
		log.Logger().Infof("this command does nothing if using github app mode and no %s option specified", optionGitHubAppOwner)
		return nil
	}

	var authConfigSvc auth.ConfigService
	if gha {
		authConfigSvc, err = o.GitAuthConfigServiceGitHubMode(o.GitKind)
		if err != nil {
			return errors.Wrap(err, "when creating auth config service using GitAuthConfigServiceGitHubMode")
		}
	} else {
		authConfigSvc, err = o.GitAuthConfigService()
		if err != nil {
			return errors.Wrap(err, "when creating auth config service using GitAuthConfigService")
		}
	}

	credentials, err := o.CreateGitCredentialsFromAuthService(authConfigSvc)
	if err != nil {
		return errors.Wrap(err, "creating git credentials")
	}
	return o.createGitCredentialsFile(outFile, credentials)
}

func (o *StepGitCredentialsOptions) GitCredentialsFileData(credentials []credentials) ([]byte, error) {
	var buffer bytes.Buffer
	for _, creds := range credentials {
		u, err := url.Parse(creds.serviceURL)
		if err != nil {
			log.Logger().Warnf("Ignoring invalid git service URL %q", creds.serviceURL)
			continue
		}

		u.User = url.UserPassword(creds.user, creds.password)
		buffer.WriteString(u.String() + "\n")
		// Write the https protocol in case only https is set for completeness
		if u.Scheme == "http" {
			u.Scheme = "https"
			buffer.WriteString(u.String() + "\n")
		}
	}

	return buffer.Bytes(), nil
}

func (o *StepGitCredentialsOptions) determineOutputFile() (string, error) {
	outFile := o.OutputFile
	if outFile == "" {
		outFile = util.GitCredentialsFile()
	}

	dir, _ := filepath.Split(outFile)
	if dir != "" {
		err := os.MkdirAll(dir, util.DefaultWritePermissions)
		if err != nil {
			return "", err
		}
	}
	return outFile, nil
}

// CreateGitCredentialsFileFromUsernameAndToken creates the git credentials into file using the provided username, token & url
func (o *StepGitCredentialsOptions) createGitCredentialsFile(fileName string, credentials []credentials) error {
	data, err := o.GitCredentialsFileData(credentials)
	if err != nil {
		return errors.Wrap(err, "creating git credentials")
	}

	if err := ioutil.WriteFile(fileName, data, util.DefaultWritePermissions); err != nil {
		return fmt.Errorf("failed to write to %s: %s", fileName, err)
	}
	log.Logger().Infof("Generated Git credentials file %s", util.ColorInfo(fileName))
	return nil
}

// CreateGitCredentialsFromAuthService creates the git credentials using the auth config service
func (o *StepGitCredentialsOptions) CreateGitCredentialsFromAuthService(authConfigSvc auth.ConfigService) ([]credentials, error) {
	var credentialList []credentials

	cfg := authConfigSvc.Config()
	if cfg == nil {
		return nil, errors.New("no git auth config found")
	}

	for _, server := range cfg.Servers {
		var auths []*auth.UserAuth
		if o.GitHubAppOwner != "" {
			auths = server.Users
		} else {
			gitAuth := server.CurrentAuth()
			if gitAuth == nil {
				continue
			} else {
				auths = append(auths, gitAuth)
			}
		}
		for _, gitAuth := range auths {
			if o.GitHubAppOwner != "" && gitAuth.GithubAppOwner != o.GitHubAppOwner {
				continue
			}
			username := gitAuth.Username
			password := gitAuth.ApiToken
			if password == "" {
				password = gitAuth.BearerToken
			}
			if password == "" {
				password = gitAuth.Password
			}
			if username == "" || password == "" {
				log.Logger().Warnf("Empty auth config for git service URL %q", server.URL)
				continue
			}

			credential := credentials{
				user:       username,
				password:   password,
				serviceURL: server.URL,
			}

			credentialList = append(credentialList, credential)
		}
	}
	return credentialList, nil
}