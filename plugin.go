package main

//TODO: This needs to be simplified a ton!
//Just the initial hack
import (
	log "github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"io"
	"io/ioutil"
	"k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"os"
	"time"
)

type (
	Config struct {
		URL        string
		Token      string
		Insecure   bool
		Namespace  string
		Template   string
		Cleanup    bool
		Timeout    int64
		KubeClient kubernetes.Interface
	}
	Build struct {
		Tag     string
		Event   string
		Number  int
		Commit  string
		Ref     string
		Branch  string
		Author  string
		Status  string
		Link    string
		Started int64
		Created int64
	}
	Job struct {
		Started int64
	}
	Repo struct {
		Owner string
		Name  string
	}
	Plugin struct {
		Repo   Repo
		Build  Build
		Config Config
		Job    Job
	}
)

func (p Plugin) Exec() error {
	log.Info("Drone k8s deployment plugin")

	if p.Config.URL == "" {
		return errors.New("eek: Must have the server url")
	}
	if p.Config.Token == "" || len(p.Config.Template) <= 0 {
		return errors.New("eek: Must have a Token")
	}
	if p.Config.Template == "" {
		return errors.New("eek: Must have a Template")
	}
	config := &rest.Config{
		Host:            p.Config.URL,
		BearerToken:     p.Config.Token,
		TLSClientConfig: rest.TLSClientConfig{Insecure: p.Config.Insecure},
	}
	//create kube client interface and add to config
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	p.Config.KubeClient = kubeClient

	// parse the template file and do substitutions
	txt, err := openAndSub(p.Config.Template, p)
	if err != nil {
		return err
	}
	json, err := utilyaml.ToJSON([]byte(txt))
	if err != nil {
		return err
	}

	//Create the job
	var jobToCreate v1.Job
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	e := runtime.DecodeInto(codecs.UniversalDecoder(), json, &jobToCreate)
	if e != nil {
		log.Fatal("Error decoding yaml file to json", e)
	}
	jobClient := p.Config.KubeClient.BatchV1().Jobs(p.Config.Namespace)
	job, err := jobClient.Create(&jobToCreate)
	if err != nil {
		return err
	}

	endMessage := make(chan error)

	//Send error to end message if timer has run out
	timeOutTimer := time.NewTimer(time.Duration(p.Config.Timeout) * time.Second)
	go func() {
		<-timeOutTimer.C
		endMessage <- errors.New("Sorry reached the timeout, You may need to manually clean up the job!")
	}()

	//Label selector based on the job
	labelSelect := labels.Set(job.Spec.Selector.MatchLabels) //might have a better way to do this?
	informerFactory := informers.
		NewSharedInformerFactoryWithOptions(p.Config.KubeClient, time.Second*30,
			informers.WithNamespace(p.Config.Namespace), informers.WithTweakListOptions(func(options *metav1.ListOptions) {
				options.LabelSelector = labelSelect.String()
			}))

	informerFactory.Batch().V1().Jobs().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			log.Info("Added ", obj)
			p.logJobContainers(informerFactory, job.Name, jobClient, endMessage)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			job, _ := newObj.(v1.Job)
			log.Info("Modified with status", job.Status)
		},
		DeleteFunc: func(obj interface{}) {
			endMessage <- errors.New("Job was delete out of band")
		},
	})
	informerFactory.Start(wait.NeverStop)

	returnError := <-endMessage
	delErr := deleteJob(job.Name, jobClient, p.Config.Cleanup)
	if delErr != nil {
		return delErr
	}
	return returnError
}

func (p Plugin) writeOutContainerLogs(podName string, writer io.Writer) error {
	req := p.Config.KubeClient.CoreV1().Pods(p.Config.Namespace).GetLogs(podName, &apiv1.PodLogOptions{
		Follow: true,
	})
	readCloser, err := req.Stream()
	if err != nil {
		return err
	}
	defer readCloser.Close()
	io.Copy(writer, readCloser)
	return nil
}

func (p Plugin) logJobContainers(informerFactory informers.SharedInformerFactory, name string, jobClient v12.JobInterface, ch chan<- error) {
	informerFactory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			log.Info("Added ", obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod, _ := newObj.(*apiv1.Pod)
			log.Info("Modified pod status", pod.Status)
			if pod.Status.Phase == apiv1.PodFailed {
				ch <- errors.New("This job failed!!!")
			}
			if pod.Status.Phase == apiv1.PodSucceeded {
				ch <- nil
			}
			if pod.Status.Phase == apiv1.PodRunning {
				err := p.writeOutContainerLogs(pod.Name, os.Stdout)
				if err != nil {
					ch <- err
				}
			}
		},
	})
	informerFactory.Start(wait.NeverStop)
}

func deleteJob(name string, jobClient v12.JobInterface, cleanup bool) error {
	if cleanup {
		delProp := metav1.DeletionPropagation(metav1.DeletePropagationForeground)
		deleteErr := jobClient.Delete(name, &metav1.DeleteOptions{
			PropagationPolicy: &delProp,
		})
		return deleteErr
	}
	return nil
}

// open up the template and then sub variables in. Handlebar stuff.
func openAndSub(templateFile string, p Plugin) (string, error) {
	t, err := ioutil.ReadFile(templateFile)
	if err != nil {
		return "", err
	}
	//potty humor!  Render trim toilet paper!  Ha ha, so funny.
	return RenderTrim(string(t), p)
}
