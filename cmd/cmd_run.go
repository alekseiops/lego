package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alekseiops/lego/v4/certificate"
	"github.com/alekseiops/lego/v4/lego"
	"github.com/alekseiops/lego/v4/log"
	"github.com/alekseiops/lego/v4/registration"
	"github.com/urfave/cli/v2"
)

func createRun() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Register an account, then create and install a certificate",
		Before: func(ctx *cli.Context) error {
			// we require either domains or csr, but not both
			hasDomains := len(ctx.StringSlice("domains")) > 0
			hasCsr := ctx.String("csr") != ""
			if hasDomains && hasCsr {
				log.Fatal("Please specify either --domains/-d or --csr/-c, but not both")
			}
			if !hasDomains && !hasCsr {
				log.Fatal("Please specify --domains/-d (or --csr/-c if you already have a CSR)")
			}
			return nil
		},
		Action: run,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "no-bundle",
				Usage: "Do not create a certificate bundle by adding the issuers certificate to the new certificate.",
			},
			&cli.BoolFlag{
				Name: "must-staple",
				Usage: "Include the OCSP must staple TLS extension in the CSR and generated certificate." +
					" Only works if the CSR is generated by lego.",
			},
			&cli.TimestampFlag{
				Name:   "not-before",
				Usage:  "Set the notBefore field in the certificate (RFC3339 format)",
				Layout: time.RFC3339,
			},
			&cli.TimestampFlag{
				Name:   "not-after",
				Usage:  "Set the notAfter field in the certificate (RFC3339 format)",
				Layout: time.RFC3339,
			},
			&cli.StringFlag{
				Name: "preferred-chain",
				Usage: "If the CA offers multiple certificate chains, prefer the chain with an issuer matching this Subject Common Name." +
					" If no match, the default offered chain will be used.",
			},
			&cli.StringFlag{
				Name:  "always-deactivate-authorizations",
				Usage: "Force the authorizations to be relinquished even if the certificate request was successful.",
			},
			&cli.StringFlag{
				Name:  "run-hook",
				Usage: "Define a hook. The hook is executed when the certificates are effectively created.",
			},
		},
	}
}

const rootPathWarningMessage = `!!!! HEADS UP !!!!

Your account credentials have been saved in your Let's Encrypt
configuration directory at "%s".

You should make a secure backup of this folder now. This
configuration directory will also contain certificates and
private keys obtained from Let's Encrypt so making regular
backups of this folder is ideal.
`

func run(ctx *cli.Context) error {
	accountsStorage := NewAccountsStorage(ctx)

	account, client := setup(ctx, accountsStorage)
	setupChallenges(ctx, client)

	if account.Registration == nil {
		reg, err := register(ctx, client)
		if err != nil {
			log.Fatalf("Could not complete registration\n\t%v", err)
		}

		account.Registration = reg
		if err = accountsStorage.Save(account); err != nil {
			log.Fatal(err)
		}

		fmt.Printf(rootPathWarningMessage, accountsStorage.GetRootPath())
	}

	certsStorage := NewCertificatesStorage(ctx)
	certsStorage.CreateRootFolder()

	cert, err := obtainCertificate(ctx, client)
	if err != nil {
		// Make sure to return a non-zero exit code if ObtainSANCertificate returned at least one error.
		// Due to us not returning partial certificate we can just exit here instead of at the end.
		log.Fatalf("Could not obtain certificates:\n\t%v", err)
	}

	certsStorage.SaveResource(cert)

	meta := map[string]string{
		renewEnvAccountEmail: account.Email,
	}

	addPathToMetadata(meta, cert.Domain, cert, certsStorage)

	return launchHook(ctx.String("run-hook"), meta)
}

func handleTOS(ctx *cli.Context, client *lego.Client) bool {
	// Check for a global accept override
	if ctx.Bool("accept-tos") {
		return true
	}

	reader := bufio.NewReader(os.Stdin)
	log.Printf("Please review the TOS at %s", client.GetToSURL())

	for {
		fmt.Println("Do you accept the TOS? Y/n")
		text, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalf("Could not read from console: %v", err)
		}

		text = strings.Trim(text, "\r\n")
		switch text {
		case "", "y", "Y":
			return true
		case "n", "N":
			return false
		default:
			fmt.Println("Your input was invalid. Please answer with one of Y/y, n/N or by pressing enter.")
		}
	}
}

func register(ctx *cli.Context, client *lego.Client) (*registration.Resource, error) {
	accepted := handleTOS(ctx, client)
	if !accepted {
		log.Fatal("You did not accept the TOS. Unable to proceed.")
	}

	if ctx.Bool("eab") {
		kid := ctx.String("kid")
		hmacEncoded := ctx.String("hmac")

		if kid == "" || hmacEncoded == "" {
			log.Fatalf("Requires arguments --kid and --hmac.")
		}

		return client.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
			TermsOfServiceAgreed: accepted,
			Kid:                  kid,
			HmacEncoded:          hmacEncoded,
		})
	}

	return client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
}

func obtainCertificate(ctx *cli.Context, client *lego.Client) (*certificate.Resource, error) {
	bundle := !ctx.Bool("no-bundle")

	domains := ctx.StringSlice("domains")
	if len(domains) > 0 {
		// obtain a certificate, generating a new private key
		request := certificate.ObtainRequest{
			Domains:                        domains,
			Bundle:                         bundle,
			MustStaple:                     ctx.Bool("must-staple"),
			PreferredChain:                 ctx.String("preferred-chain"),
			AlwaysDeactivateAuthorizations: ctx.Bool("always-deactivate-authorizations"),
		}

		notBefore := ctx.Timestamp("not-before")
		if notBefore != nil {
			request.NotBefore = *notBefore
		}

		notAfter := ctx.Timestamp("not-after")
		if notAfter != nil {
			request.NotAfter = *notAfter
		}

		return client.Certificate.Obtain(request)
	}

	// read the CSR
	csr, err := readCSRFile(ctx.String("csr"))
	if err != nil {
		return nil, err
	}

	// obtain a certificate for this CSR
	request := certificate.ObtainForCSRRequest{
		CSR:                            csr,
		NotBefore:                      getTime(ctx, "not-before"),
		NotAfter:                       getTime(ctx, "not-after"),
		Bundle:                         bundle,
		PreferredChain:                 ctx.String("preferred-chain"),
		AlwaysDeactivateAuthorizations: ctx.Bool("always-deactivate-authorizations"),
	}

	return client.Certificate.ObtainForCSR(request)
}
