package natsauthcallout

import (
	"context"
	"fmt"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type (
	// KubeTokenReviewer validates tokens via the Kubernetes TokenReview API.
	KubeTokenReviewer struct {
		client kubernetes.Interface
	}
)

// NewInClusterTokenReviewer builds a TokenReviewer from in-cluster config.
func NewInClusterTokenReviewer() (*KubeTokenReviewer, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	return &KubeTokenReviewer{client: client}, nil
}

// Review submits a TokenReview and returns its status.
func (r *KubeTokenReviewer) Review(ctx context.Context, token string, audiences []string) (*authv1.TokenReviewStatus, error) {
	review := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: audiences,
		},
	}

	result, err := r.client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create tokenreview: %w", err)
	}

	return &result.Status, nil
}
