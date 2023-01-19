package manager

import (
	"context"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

func (sm *Manager) watchEndpoint(ctx context.Context, id string, service *v1.Service, wg *sync.WaitGroup) error {
	log.Infof("[endpoint] watching for service [%s] in namespace [%s]", service.Name, service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	var cancel context.CancelFunc
	var endpointContext context.Context
	endpointContext, cancel = context.WithCancel(context.Background())
	var electionActive bool
	defer cancel()

	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", service.Name).String(),
	}
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Endpoints(service.Namespace).Watch(ctx, opts)
		},
	})
	if err != nil {
		cancel()
		return fmt.Errorf("error creating endpoint watcher: %s", err.Error())
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-sm.shutdownChan:
			log.Debug("[endpoint] shutdown called")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("[endpoint] function ending")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	var lastKnownGoodEndpoint string
	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			ep, ok := event.Object.(*v1.Endpoints)
			if !ok {
				cancel()
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}
			// Build endpoints
			var localendpoints []string
			for subset := range ep.Subsets {
				for address := range ep.Subsets[subset].Addresses {

					// Check the node is populated
					if ep.Subsets[subset].Addresses[address].NodeName != nil {
						if id == *ep.Subsets[subset].Addresses[address].NodeName {
							localendpoints = append(localendpoints, ep.Subsets[subset].Addresses[address].IP)
						}
					}
				}
			}
			log.Debugf("[endpoint watcher] local endpoint(s) [%d], last known good [%s], active election [%t]", len(localendpoints), lastKnownGoodEndpoint, electionActive)

			stillExists := false
			if len(localendpoints) != 0 {
				if lastKnownGoodEndpoint == "" {
					lastKnownGoodEndpoint = localendpoints[0]
					// Create new context
					//endpointContext, cancel = context.WithCancel(context.Background()) //nolint:govet
					//defer cancel()                                                     //nolint
					wg.Add(1)
					if service.Annotations["kube-vip.io/egress"] == "true" {
						service.Annotations["kube-vip.io/active-endpoint"] = lastKnownGoodEndpoint
					}
				} else {
					// check out previous endpoint exists
					for x := range localendpoints {
						if localendpoints[x] == lastKnownGoodEndpoint {
							stillExists = true
						}
					}
					if stillExists {
						break
					} else {
						cancel()
						//rw.Stop()
					}
				}
				if !electionActive {
					go func() {
						// This is a blocking function, that will restart (in the event of failure)
						for {
							// if the context isn't cancelled restart
							if endpointContext.Err() != context.Canceled {
								electionActive = true
								err = sm.StartServicesLeaderElection(endpointContext, service, wg)
								electionActive = false
								if err != nil {
									log.Error(err)
								}
							} else {
								electionActive = false
								break
							}
						}
						wg.Done()
					}()
				}
			} else {
				if lastKnownGoodEndpoint != "" {
					lastKnownGoodEndpoint = ""
					cancel()
					//rw.Stop()
					//return nil
				}
			}

		case watch.Deleted:
			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Infof("[endpoints] deleted stopping watching for [%s] in namespace [%s]", service.Name, service.Namespace)
			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Errorf("endpoint -> %v", statusErr)
		}
	}
	close(exitFunction)
	log.Infof("[endpoints] stopping watching for [%s] in namespace [%s]", service.Name, service.Namespace)
	return nil //nolint:govet
}

func (sm *Manager) updateServiceEndpointAnnotation(endpoint string, service *v1.Service) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// id, err := os.Hostname()
		// if err != nil {
		// 	return err
		// }

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		currentServiceCopy.Annotations["kube-vip.io/active-endpoint"] = endpoint

		_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service Spec [%s] : %v", currentServiceCopy.Name, err)
			return err
		}

		// updatedService.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: i.vipConfig.VIP}}
		// _, err = sm.clientSet.CoreV1().Services(updatedService.Namespace).UpdateStatus(context.TODO(), updatedService, metav1.UpdateOptions{})
		// if err != nil {
		// 	log.Errorf("Error updating Service %s/%s Status: %v", i.ServiceNamespace, i.ServiceName, err)
		// 	return err
		// }
		return nil
	})

	if retryErr != nil {
		log.Errorf("Failed to set Services: %v", retryErr)
		return retryErr
	}
	return nil
}
