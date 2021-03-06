package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/containers/image/v5/pkg/compression"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
	"github.com/prometheus/common/log"
	"github.com/urfave/cli"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var (
	yamlSep              = regexp.MustCompile(`\n---`)
	extraImageAnnotation = "skopeo.io/extraimages"
)

// errorShouldDisplayUsage is a subtype of error used by command handlers to indicate that cli.ShowSubcommandHelp should be called.
type errorShouldDisplayUsage struct {
	error
}

// commandAction intermediates between the cli.ActionFunc interface and the real handler,
// primarily to ensure that cli.Context is not available to the handler, which in turn
// makes sure that the cli.String() etc. flag access functions are not used,
// and everything is done using the *Options structures and the Destination: members of cli.Flag.
// handler may return errorShouldDisplayUsage to cause cli.ShowSubcommandHelp to be called.
func commandAction(handler func(args []string, stdout io.Writer) error) cli.ActionFunc {
	return func(c *cli.Context) error {
		err := handler(([]string)(c.Args()), c.App.Writer)
		if _, ok := err.(errorShouldDisplayUsage); ok {
			cli.ShowSubcommandHelp(c)
		}
		return err
	}
}

// sharedImageOptions collects CLI flags which are image-related, but do not change across images.
// This really should be a part of globalOptions, but that would break existing users of (skopeo copy --authfile=).
type sharedImageOptions struct {
	authFilePath string // Path to a */containers/auth.json
}

// imageFlags prepares a collection of CLI flags writing into sharedImageOptions, and the managed sharedImageOptions structure.
func sharedImageFlags() ([]cli.Flag, *sharedImageOptions) {
	opts := sharedImageOptions{}
	return []cli.Flag{
		cli.StringFlag{
			Name:        "authfile",
			Usage:       "path of the authentication file. Default is ${XDG_RUNTIME_DIR}/containers/auth.json",
			Destination: &opts.authFilePath,
		},
	}, &opts
}

// imageOptions collects CLI flags specific to the "docker" transport, which are
// the same across subcommands, but may be different for each image
// (e.g. may differ between the source and destination of a copy)
type dockerImageOptions struct {
	global         *globalOptions      // May be shared across several imageOptions instances.
	shared         *sharedImageOptions // May be shared across several imageOptions instances.
	authFilePath   optionalString      // Path to a */containers/auth.json (prefixed version to override shared image option).
	credsOption    optionalString      // username[:password] for accessing a registry
	dockerCertPath string              // A directory using Docker-like *.{crt,cert,key} files for connecting to a registry or a daemon
	tlsVerify      optionalBool        // Require HTTPS and verify certificates (for docker: and docker-daemon:)
	noCreds        bool                // Access the registry anonymously
}

// imageOptions collects CLI flags which are the same across subcommands, but may be different for each image
// (e.g. may differ between the source and destination of a copy)
type imageOptions struct {
	dockerImageOptions
	sharedBlobDir    string // A directory to use for OCI blobs, shared across repositories
	dockerDaemonHost string // docker-daemon: host to connect to
}

// dockerImageFlags prepares a collection of docker-transport specific CLI flags
// writing into imageOptions, and the managed imageOptions structure.
func dockerImageFlags(global *globalOptions, shared *sharedImageOptions, flagPrefix, credsOptionAlias string) ([]cli.Flag, *imageOptions) {
	opts := imageOptions{
		dockerImageOptions: dockerImageOptions{
			global: global,
			shared: shared,
		},
	}

	// This is horribly ugly, but we need to support the old option forms of (skopeo copy) for compatibility.
	// Don't add any more cases like this.
	credsOptionExtra := ""
	if credsOptionAlias != "" {
		credsOptionExtra += "," + credsOptionAlias
	}

	var flags []cli.Flag
	if flagPrefix != "" {
		// the non-prefixed flag is handled by a shared flag.
		flags = append(flags,
			cli.GenericFlag{
				Name:  flagPrefix + "authfile",
				Usage: "path of the authentication file. Default is ${XDG_RUNTIME_DIR}/containers/auth.json",
				Value: newOptionalStringValue(&opts.authFilePath),
			},
		)
	}
	flags = append(flags,
		cli.GenericFlag{
			Name:  flagPrefix + "creds" + credsOptionExtra,
			Usage: "Use `USERNAME[:PASSWORD]` for accessing the registry",
			Value: newOptionalStringValue(&opts.credsOption),
		},
		cli.StringFlag{
			Name:        flagPrefix + "cert-dir",
			Usage:       "use certificates at `PATH` (*.crt, *.cert, *.key) to connect to the registry or daemon",
			Destination: &opts.dockerCertPath,
		},
		cli.GenericFlag{
			Name:  flagPrefix + "tls-verify",
			Usage: "require HTTPS and verify certificates when talking to the container registry or daemon (defaults to true)",
			Value: newOptionalBoolValue(&opts.tlsVerify),
		},
		cli.BoolFlag{
			Name:        flagPrefix + "no-creds",
			Usage:       "Access the registry anonymously",
			Destination: &opts.noCreds,
		},
	)
	return flags, &opts
}

// imageFlags prepares a collection of CLI flags writing into imageOptions, and the managed imageOptions structure.
func imageFlags(global *globalOptions, shared *sharedImageOptions, flagPrefix, credsOptionAlias string) ([]cli.Flag, *imageOptions) {
	dockerFlags, opts := dockerImageFlags(global, shared, flagPrefix, credsOptionAlias)

	return append(dockerFlags, []cli.Flag{
		cli.StringFlag{
			Name:        flagPrefix + "shared-blob-dir",
			Usage:       "`DIRECTORY` to use to share blobs across OCI repositories",
			Destination: &opts.sharedBlobDir,
		},
		cli.StringFlag{
			Name:        flagPrefix + "daemon-host",
			Usage:       "use docker daemon host at `HOST` (docker-daemon: only)",
			Destination: &opts.dockerDaemonHost,
		},
	}...), opts
}

// newSystemContext returns a *types.SystemContext corresponding to opts.
// It is guaranteed to return a fresh instance, so it is safe to make additional updates to it.
func (opts *imageOptions) newSystemContext() (*types.SystemContext, error) {
	ctx := &types.SystemContext{
		RegistriesDirPath:        opts.global.registriesDirPath,
		ArchitectureChoice:       opts.global.overrideArch,
		OSChoice:                 opts.global.overrideOS,
		DockerCertPath:           opts.dockerCertPath,
		OCISharedBlobDirPath:     opts.sharedBlobDir,
		AuthFilePath:             opts.shared.authFilePath,
		DockerDaemonHost:         opts.dockerDaemonHost,
		DockerDaemonCertPath:     opts.dockerCertPath,
		SystemRegistriesConfPath: opts.global.registriesConfPath,
	}
	if opts.dockerImageOptions.authFilePath.present {
		ctx.AuthFilePath = opts.dockerImageOptions.authFilePath.value
	}
	if opts.tlsVerify.present {
		ctx.DockerDaemonInsecureSkipTLSVerify = !opts.tlsVerify.value
	}
	// DEPRECATED: We support this for backward compatibility, but override it if a per-image flag is provided.
	if opts.global.tlsVerify.present {
		ctx.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!opts.global.tlsVerify.value)
	}
	if opts.tlsVerify.present {
		ctx.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!opts.tlsVerify.value)
	}
	if opts.credsOption.present && opts.noCreds {
		return nil, errors.New("creds and no-creds cannot be specified at the same time")
	}
	if opts.credsOption.present {
		var err error
		ctx.DockerAuthConfig, err = getDockerAuth(opts.credsOption.value)
		if err != nil {
			return nil, err
		}
	}
	if opts.noCreds {
		ctx.DockerAuthConfig = &types.DockerAuthConfig{}
	}

	return ctx, nil
}

// imageDestOptions is a superset of imageOptions specialized for iamge destinations.
type imageDestOptions struct {
	*imageOptions
	dirForceCompression         bool        // Compress layers when saving to the dir: transport
	ociAcceptUncompressedLayers bool        // Whether to accept uncompressed layers in the oci: transport
	compressionFormat           string      // Format to use for the compression
	compressionLevel            optionalInt // Level to use for the compression
}

// imageDestFlags prepares a collection of CLI flags writing into imageDestOptions, and the managed imageDestOptions structure.
func imageDestFlags(global *globalOptions, shared *sharedImageOptions, flagPrefix, credsOptionAlias string) ([]cli.Flag, *imageDestOptions) {
	genericFlags, genericOptions := imageFlags(global, shared, flagPrefix, credsOptionAlias)
	opts := imageDestOptions{imageOptions: genericOptions}

	return append(genericFlags, []cli.Flag{
		cli.BoolFlag{
			Name:        flagPrefix + "compress",
			Usage:       "Compress tarball image layers when saving to directory using the 'dir' transport. (default is same compression type as source)",
			Destination: &opts.dirForceCompression,
		},
		cli.BoolFlag{
			Name:        flagPrefix + "oci-accept-uncompressed-layers",
			Usage:       "Allow uncompressed image layers when saving to an OCI image using the 'oci' transport. (default is to compress things that aren't compressed)",
			Destination: &opts.ociAcceptUncompressedLayers,
		},
		cli.StringFlag{
			Name:        flagPrefix + "compress-format",
			Usage:       "`FORMAT` to use for the compression",
			Destination: &opts.compressionFormat,
		},
		cli.GenericFlag{
			Name:  flagPrefix + "compress-level",
			Usage: "`LEVEL` to use for the compression",
			Value: newOptionalIntValue(&opts.compressionLevel),
		},
	}...), &opts
}

// newSystemContext returns a *types.SystemContext corresponding to opts.
// It is guaranteed to return a fresh instance, so it is safe to make additional updates to it.
func (opts *imageDestOptions) newSystemContext() (*types.SystemContext, error) {
	ctx, err := opts.imageOptions.newSystemContext()
	if err != nil {
		return nil, err
	}

	ctx.DirForceCompress = opts.dirForceCompression
	ctx.OCIAcceptUncompressedLayers = opts.ociAcceptUncompressedLayers
	if opts.compressionFormat != "" {
		cf, err := compression.AlgorithmByName(opts.compressionFormat)
		if err != nil {
			return nil, err
		}
		ctx.CompressionFormat = &cf
	}
	if opts.compressionLevel.present {
		ctx.CompressionLevel = &opts.compressionLevel.value
	}
	return ctx, err
}

func parseCreds(creds string) (string, string, error) {
	if creds == "" {
		return "", "", errors.New("credentials can't be empty")
	}
	up := strings.SplitN(creds, ":", 2)
	if len(up) == 1 {
		return up[0], "", nil
	}
	if up[0] == "" {
		return "", "", errors.New("username can't be empty")
	}
	return up[0], up[1], nil
}

func getDockerAuth(creds string) (*types.DockerAuthConfig, error) {
	username, password, err := parseCreds(creds)
	if err != nil {
		return nil, err
	}
	return &types.DockerAuthConfig{
		Username: username,
		Password: password,
	}, nil
}

// parseImage converts image URL-like string to an initialized handler for that image.
// The caller must call .Close() on the returned ImageCloser.
func parseImage(ctx context.Context, opts *imageOptions, name string) (types.ImageCloser, error) {
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		return nil, err
	}
	sys, err := opts.newSystemContext()
	if err != nil {
		return nil, err
	}
	return ref.NewImage(ctx, sys)
}

// parseImageSource converts image URL-like string to an ImageSource.
// The caller must call .Close() on the returned ImageSource.
func parseImageSource(ctx context.Context, opts *imageOptions, name string) (types.ImageSource, error) {
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		return nil, err
	}
	sys, err := opts.newSystemContext()
	if err != nil {
		return nil, err
	}
	return ref.NewImageSource(ctx, sys)
}

// UnmarshalUnstructuredYAML takes in a YAML data as a string and returns a list of metav1.unstructured type objects
func unmarshalUnstructuredK8s(data string) (objs []*unstructured.Unstructured) {
	// Split the yaml data into parts based off the yaml sep
	parts := yamlSep.Split(data, -1)

	// Loop over each part and unmarshal
	for _, part := range parts {
		// Check for empty yaml
		if len(part) == 0 {
			log.Debugf("Empty manifests found, skipping...")
			continue
		}

		var obj unstructured.Unstructured
		err := yaml.Unmarshal([]byte(part), &obj)
		if err != nil {
			log.Fatalf("Failed to unmarshal map into unstructured resource: %v", err)
		}

		objs = append(objs, &obj)
	}
	return
}

func parseImagesFromManifests(yaml string) (images []string) {
	objs := unmarshalUnstructuredK8s(yaml)

	for _, obj := range objs {
		// Loop through every yaml obj recursively until done
		images = append(images, walkImage(obj.Object)...)

		// Get any annotations
		for k, v := range obj.GetAnnotations() {
			// If we stumble upon the right annotation in a resource, add extra images to the list
			if k == extraImageAnnotation {
				images = append(images, strings.Split(v, ",")...)
			}
		}
	}
	return
}

func walkImage(obj map[string]interface{}) (images []string) {
	for k, v := range obj {

		// If we're at an array (such as the ones that containers are stored at)
		if array, ok := v.([]interface{}); ok {
			// containers are the only thing that have images so they're the only things we care about (in this search)
			if k == "containers" || k == "initContainers" {
				for _, obj := range array {
					if mapObj, isMap := obj.(map[string]interface{}); isMap {
						if image, isImage := mapObj["image"]; isImage {
							// Convert (to string) and append to list of images
							images = append(images, fmt.Sprintf("%s", image))
						}
					}
				}
			}
		} else if objMap, ok := v.(map[string]interface{}); ok {
			// Keep digging until we run out of maps or find a container
			images = append(images, walkImage(objMap)...)
		}
	}

	return
}
