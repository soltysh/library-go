package registryclient

import (
	"context"
	"fmt"
	"io/ioutil"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/docker/distribution"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1scheme "github.com/openshift/client-go/operator/clientset/versioned/scheme"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	imagereference "github.com/openshift/library-go/pkg/image/reference"
)

// AlternativeImageSources holds ImageContentSourcePolicy variables to look up image sources
type AlternativeImageSources struct {
	icspFile     string
	icspList     []operatorv1alpha1.ImageContentSourcePolicy
	icspClientFn func() (operatorv1alpha1client.ImageContentSourcePolicyInterface, error)
}

// WithICSP returns a Context with ImageContentSourcePolicy variables populated
func (c *Context) WithICSP(icspFile string, icspClientFn func() (operatorv1alpha1client.ImageContentSourcePolicyInterface, error)) *Context {
	c.ImageSources = AlternativeImageSources{
		icspFile:     icspFile,
		icspClientFn: icspClientFn,
	}
	return c
}

// addICSPsFromCluster will lookup ICSPs from cluster. Logs errors rather than returning errors. By design,
// this function has no way of knowing whether it expects to connect to a cluster or not. That is up the the caller.
func (c *Context) addICSPsFromCluster() error {
	icspClient, err := c.ImageSources.icspClientFn()
	if err != nil || icspClient == nil {
		return fmt.Errorf("no client to access ImageContentSourcePolicies in cluster: %v", err)
	}
	icsps, err := icspClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		// may or may not have access to ICSPs in cluster
		// don't error if can't access ICSPs
		return fmt.Errorf("did not access any ImageContentSourcePolicies in cluster: %v", err)
	}
	if len(icsps.Items) == 0 {
		// might be looking up implicitly, log only
		klog.V(4).Info("no ImageContentSourcePolicies found in cluster")
	}
	c.ImageSources.icspList = append(c.ImageSources.icspList, icsps.Items...)
	return nil
}

// addImageSourcePoliciesFromFile appends to list of alternative image sources from ICSP file
// returns error if no icsp object decoded from file data
func (c *Context) addImageSourcePoliciesFromFile() error {
	if len(c.ImageSources.icspFile) == 0 {
		return nil
	}
	icspData, err := ioutil.ReadFile(c.ImageSources.icspFile)
	if err != nil {
		return fmt.Errorf("unable to read ImageContentSourceFile %s: %v", c.ImageSources.icspFile, err)
	}
	if len(icspData) == 0 {
		return fmt.Errorf("no data found in ImageContentSourceFile %s", c.ImageSources.icspFile)
	}
	icspObj, err := runtime.Decode(operatorv1alpha1scheme.Codecs.UniversalDeserializer(), icspData)
	if err != nil {
		return fmt.Errorf("error decoding ImageContentSourcePolicy from %s: %v", c.ImageSources.icspFile, err)
	}
	icsp, ok := icspObj.(*operatorv1alpha1.ImageContentSourcePolicy)
	if !ok {
		return fmt.Errorf("could not decode ImageContentSourcePolicy from %s", c.ImageSources.icspFile)
	}
	c.ImageSources.icspList = append(c.ImageSources.icspList, *icsp)
	return nil
}

// alternativeImageSources returns list of DockerImageReference objects from list of ImageContentSourcePolicy objects
func (a *Context) alternativeImageSources(imageRef imagereference.DockerImageReference) ([]imagereference.DockerImageReference, error) {
	var imageSources []imagereference.DockerImageReference
	for _, icsp := range a.ImageSources.icspList {
		repoDigestMirrors := icsp.Spec.RepositoryDigestMirrors
		var sourceMatches bool
		for _, rdm := range repoDigestMirrors {
			rdmRef, err := imagereference.Parse(rdm.Source)
			if err != nil {
				return nil, err
			}
			if imageRef.AsRepository() == rdmRef.AsRepository() {
				klog.V(2).Infof("%v RepositoryDigestMirrors source matches given image", imageRef.AsRepository())
				sourceMatches = true
			}
			for _, m := range rdm.Mirrors {
				if sourceMatches {
					klog.V(2).Infof("%v RepositoryDigestMirrors mirror added to potential ImageSourcePrefixes from ImageContentSourcePolicy", m)
					mRef, err := imagereference.Parse(m)
					if err != nil {
						return nil, err
					}
					imageSources = append(imageSources, mRef)
				}
			}
		}
	}
	uniqueMirrors := make([]imagereference.DockerImageReference, 0, len(imageSources))
	uniqueMap := make(map[imagereference.DockerImageReference]bool)
	for _, imageSourceMirror := range imageSources {
		if _, ok := uniqueMap[imageSourceMirror]; !ok {
			uniqueMap[imageSourceMirror] = true
			uniqueMirrors = append(uniqueMirrors, imageSourceMirror)
		}
	}
	// make sure at least 1 imagesource
	// ie, make sure the image passed is included in image sources
	// this is so the user-given image ref will be tried
	if len(imageSources) == 0 {
		imageSources = append(imageSources, imageRef.AsRepository())
		return imageSources, nil
	}
	klog.V(2).Infof("Found sources: %v for image: %v", uniqueMirrors, imageRef)
	return uniqueMirrors, nil
}

// PreferredImageSource chooses appropriate DockerImageReference for a Context from possible image sources gathered from
// ImageContentSourcePolicy objects and user-passed image. PreferredImageSource will lookup from cluster
// if alwaysTryAlternativeSources is true, otherwise will lookup from file if file given, or from
// original image-reference of user-given image (that may be different from user-given in case of mirrored images).
func (c *Context) PreferredImageSource(image imagereference.DockerImageReference, alwaysTryAlternativeSources, insecure bool) (imagereference.DockerImageReference, distribution.Repository, distribution.ManifestService, error) {
	ctx := context.TODO()
	// alwaysTryAlternativeSources is false when don't want implicit alternative image source lookup
	// icspFile indicates explicit alternative image source lookup
	if !alwaysTryAlternativeSources {
		repo, err := c.Repository(ctx, image.DockerClientDefaults().RegistryURL(), image.RepositoryName(), insecure)
		if err != nil {
			return imagereference.DockerImageReference{}, nil, nil, err
		}
		manifests, err := repo.Manifests(ctx)
		return image, repo, manifests, err
	}
	if err := c.addImageSourcePoliciesFromFile(); err != nil {
		return imagereference.DockerImageReference{}, nil, nil, err
	}
	if err := c.addICSPsFromCluster(); err != nil {
		return imagereference.DockerImageReference{}, nil, nil, err
	}
	altSources, err := c.alternativeImageSources(image)
	if err != nil {
		return imagereference.DockerImageReference{}, nil, nil, err
	}
	for _, icsRef := range altSources {
		replacedImage := replaceImage(icsRef, image)
		if repo, err := c.Repository(ctx, replacedImage.DockerClientDefaults().RegistryURL(), replacedImage.RepositoryName(), insecure); err == nil {
			manifests, err := repo.Manifests(ctx)
			return replacedImage, repo, manifests, err
		}
	}
	return imagereference.DockerImageReference{}, nil, nil, nil
}

func replaceImage(icsRef imagereference.DockerImageReference, image imagereference.DockerImageReference) imagereference.DockerImageReference {
	icsRef.ID = image.ID
	icsRef.Tag = image.Tag
	return icsRef
}
