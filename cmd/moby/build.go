package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
)

const defaultNameForStdin = "moby"

type outputList []string

func (o *outputList) String() string {
	return fmt.Sprint(*o)
}

func (o *outputList) Set(value string) error {
	// allow comma seperated options or multiple options
	for _, cs := range strings.Split(value, ",") {
		*o = append(*o, cs)
	}
	return nil
}

// Process the build arguments and execute build
func build(args []string) {
	var buildOut outputList

	outputTypes := []string{}
	for k := range outFuns {
		outputTypes = append(outputTypes, k)
	}
	sort.Strings(outputTypes)

	buildCmd := flag.NewFlagSet("build", flag.ExitOnError)
	buildCmd.Usage = func() {
		fmt.Printf("USAGE: %s build [options] <file>[.yml] | -\n\n", os.Args[0])
		fmt.Printf("Options:\n")
		buildCmd.PrintDefaults()
	}
	buildName := buildCmd.String("name", "", "Name to use for output files")
	buildDir := buildCmd.String("dir", "", "Directory for output files, default current directory")
	buildSize := buildCmd.String("size", "1024M", "Size for output image, if supported and fixed size")
	buildPull := buildCmd.Bool("pull", false, "Always pull images")
	buildDisableTrust := buildCmd.Bool("disable-content-trust", false, "Skip image trust verification specified in trust section of config (default false)")
	buildHyperkit := buildCmd.Bool("hyperkit", false, "Use hyperkit for LinuxKit based builds where possible")
	buildCmd.Var(&buildOut, "output", "Output types to create [ "+strings.Join(outputTypes, " ")+" ]")

	if err := buildCmd.Parse(args); err != nil {
		log.Fatal("Unable to parse args")
	}
	remArgs := buildCmd.Args()

	if len(remArgs) == 0 {
		fmt.Println("Please specify a configuration file")
		buildCmd.Usage()
		os.Exit(1)
	}

	if len(buildOut) == 0 {
		buildOut = outputList{"kernel+initrd"}
	}

	log.Debugf("Outputs selected: %s", buildOut.String())

	err := validateOutputs(buildOut)
	if err != nil {
		log.Errorf("Error parsing outputs: %v", err)
		buildCmd.Usage()
		os.Exit(1)
	}

	size, err := getDiskSizeMB(*buildSize)
	if err != nil {
		log.Fatalf("Unable to parse disk size: %v", err)
	}

	name := *buildName
	var config []byte
	if conf := remArgs[0]; conf == "-" {
		var err error
		config, err = ioutil.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("Cannot read stdin: %v", err)
		}
		if name == "" {
			name = defaultNameForStdin
		}
	} else {
		if !(filepath.Ext(conf) == ".yml" || filepath.Ext(conf) == ".yaml") {
			conf = conf + ".yml"
		}
		var err error
		config, err = ioutil.ReadFile(conf)
		if err != nil {
			log.Fatalf("Cannot open config file: %v", err)
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(conf), filepath.Ext(conf))
		}
	}

	m, err := NewConfig(config)
	if err != nil {
		log.Fatalf("Invalid config: %v", err)
	}

	if *buildDisableTrust {
		log.Debugf("Disabling content trust checks for this build")
		m.Trust = TrustConfig{}
	}

	image := buildInternal(m, *buildPull)

	log.Infof("Create outputs:")
	err = outputs(filepath.Join(*buildDir, name), image, buildOut, size, *buildHyperkit)
	if err != nil {
		log.Fatalf("Error writing outputs: %v", err)
	}
}

// Parse a string which is either a number in MB, or a number with
// either M (for Megabytes) or G (for GigaBytes) as a suffix and
// returns the number in MB. Return 0 if string is empty.
func getDiskSizeMB(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	sz := len(s)
	if strings.HasSuffix(s, "G") {
		i, err := strconv.Atoi(s[:sz-1])
		if err != nil {
			return 0, err
		}
		return i * 1024, nil
	}
	if strings.HasSuffix(s, "M") {
		s = s[:sz-1]
	}
	return strconv.Atoi(s)
}

func initrdAppend(iw *tar.Writer, r io.Reader) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalln(err)
		}
		err = iw.WriteHeader(hdr)
		if err != nil {
			log.Fatalln(err)
		}
		_, err = io.Copy(iw, tr)
		if err != nil {
			log.Fatalln(err)
		}
	}
}

func enforceContentTrust(fullImageName string, config *TrustConfig) bool {
	for _, img := range config.Image {
		// First check for an exact name match
		if img == fullImageName {
			return true
		}
		// Also check for an image name only match
		// by removing a possible tag (with possibly added digest):
		imgAndTag := strings.Split(fullImageName, ":")
		if len(imgAndTag) >= 2 && img == imgAndTag[0] {
			return true
		}
		// and by removing a possible digest:
		imgAndDigest := strings.Split(fullImageName, "@sha256:")
		if len(imgAndDigest) >= 2 && img == imgAndDigest[0] {
			return true
		}
	}

	for _, org := range config.Org {
		var imgOrg string
		splitName := strings.Split(fullImageName, "/")
		switch len(splitName) {
		case 0:
			// if the image is empty, return false
			return false
		case 1:
			// for single names like nginx, use library
			imgOrg = "library"
		case 2:
			// for names that assume docker hub, like linxukit/alpine, take the first split
			imgOrg = splitName[0]
		default:
			// for names that include the registry, the second piece is the org, ex: docker.io/library/alpine
			imgOrg = splitName[1]
		}
		if imgOrg == org {
			return true
		}
	}
	return false
}

// Perform the actual build process
// TODO return error not panic
func buildInternal(m Moby, pull bool) []byte {
	w := new(bytes.Buffer)
	iw := tar.NewWriter(w)

	if pull || enforceContentTrust(m.Kernel.Image, &m.Trust) {
		log.Infof("Pull kernel image: %s", m.Kernel.Image)
		err := dockerPull(m.Kernel.Image, enforceContentTrust(m.Kernel.Image, &m.Trust))
		if err != nil {
			log.Fatalf("Could not pull image %s: %v", m.Kernel.Image, err)
		}
	}
	if m.Kernel.Image != "" {
		// get kernel and initrd tarball from container
		log.Infof("Extract kernel image: %s", m.Kernel.Image)
		const (
			kernelName    = "kernel"
			kernelAltName = "bzImage"
			ktarName      = "kernel.tar"
		)
		out, err := ImageExtract(m.Kernel.Image, "", enforceContentTrust(m.Kernel.Image, &m.Trust), pull)
		if err != nil {
			log.Fatalf("Failed to extract kernel image and tarball: %v", err)
		}
		buf := bytes.NewBuffer(out)

		kernel, ktar, err := untarKernel(buf, kernelName, kernelAltName, ktarName, m.Kernel.Cmdline)
		if err != nil {
			log.Fatalf("Could not extract kernel image and filesystem from tarball. %v", err)
		}
		initrdAppend(iw, kernel)
		initrdAppend(iw, ktar)
	}

	// convert init images to tarballs
	if len(m.Init) != 0 {
		log.Infof("Add init containers:")
	}
	for _, ii := range m.Init {
		log.Infof("Process init image: %s", ii)
		init, err := ImageExtract(ii, "", enforceContentTrust(ii, &m.Trust), pull)
		if err != nil {
			log.Fatalf("Failed to build init tarball from %s: %v", ii, err)
		}
		buffer := bytes.NewBuffer(init)
		initrdAppend(iw, buffer)
	}

	if len(m.Onboot) != 0 {
		log.Infof("Add onboot containers:")
	}
	for i, image := range m.Onboot {
		log.Infof("  Create OCI config for %s", image.Image)
		config, err := ConfigToOCI(image)
		if err != nil {
			log.Fatalf("Failed to create config.json for %s: %v", image.Image, err)
		}
		so := fmt.Sprintf("%03d", i)
		path := "containers/onboot/" + so + "-" + image.Name
		out, err := ImageBundle(path, image.Image, config, enforceContentTrust(image.Image, &m.Trust), pull)
		if err != nil {
			log.Fatalf("Failed to extract root filesystem for %s: %v", image.Image, err)
		}
		buffer := bytes.NewBuffer(out)
		initrdAppend(iw, buffer)
	}

	if len(m.Services) != 0 {
		log.Infof("Add service containers:")
	}
	for _, image := range m.Services {
		log.Infof("  Create OCI config for %s", image.Image)
		config, err := ConfigToOCI(image)
		if err != nil {
			log.Fatalf("Failed to create config.json for %s: %v", image.Image, err)
		}
		path := "containers/services/" + image.Name
		out, err := ImageBundle(path, image.Image, config, enforceContentTrust(image.Image, &m.Trust), pull)
		if err != nil {
			log.Fatalf("Failed to extract root filesystem for %s: %v", image.Image, err)
		}
		buffer := bytes.NewBuffer(out)
		initrdAppend(iw, buffer)
	}

	// add files
	buffer, err := filesystem(m)
	if err != nil {
		log.Fatalf("failed to add filesystem parts: %v", err)
	}
	initrdAppend(iw, buffer)
	err = iw.Close()
	if err != nil {
		log.Fatalf("initrd close error: %v", err)
	}

	return w.Bytes()
}

func untarKernel(buf *bytes.Buffer, kernelName, kernelAltName, ktarName string, cmdline string) (*bytes.Buffer, *bytes.Buffer, error) {
	tr := tar.NewReader(buf)

	var kernel, ktar *bytes.Buffer
	foundKernel := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalln(err)
		}
		switch hdr.Name {
		case kernelName, kernelAltName:
			if foundKernel {
				return nil, nil, errors.New("found more than one possible kernel image")
			}
			foundKernel = true
			kernel = new(bytes.Buffer)
			// make a new tarball with kernel in /boot/kernel
			tw := tar.NewWriter(kernel)
			whdr := &tar.Header{
				Name:     "boot",
				Mode:     0700,
				Typeflag: tar.TypeDir,
			}
			if err := tw.WriteHeader(whdr); err != nil {
				return nil, nil, err
			}
			whdr = &tar.Header{
				Name: "boot/kernel",
				Mode: hdr.Mode,
				Size: hdr.Size,
			}
			if err := tw.WriteHeader(whdr); err != nil {
				return nil, nil, err
			}
			_, err = io.Copy(tw, tr)
			if err != nil {
				return nil, nil, err
			}
			// add the cmdline in /boot/cmdline
			whdr = &tar.Header{
				Name: "boot/cmdline",
				Mode: 0700,
				Size: int64(len(cmdline)),
			}
			if err := tw.WriteHeader(whdr); err != nil {
				return nil, nil, err
			}
			buf := bytes.NewBufferString(cmdline)
			_, err = io.Copy(tw, buf)
			if err != nil {
				return nil, nil, err
			}
			if err := tw.Close(); err != nil {
				return nil, nil, err
			}
		case ktarName:
			ktar = new(bytes.Buffer)
			_, err := io.Copy(ktar, tr)
			if err != nil {
				return nil, nil, err
			}
		default:
			continue
		}
	}

	if kernel == nil {
		return nil, nil, errors.New("did not find kernel in kernel image")
	}
	if ktar == nil {
		return nil, nil, errors.New("did not find kernel.tar in kernel image")
	}

	return kernel, ktar, nil
}
