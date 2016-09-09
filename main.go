package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli"

	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
)

var VERSION string

func createInstanceTemplateWithImage(service *compute.Service, im *compute.Image, itToClone, project string) (*compute.InstanceTemplate, error) {

	desiredIt := fmt.Sprintf("%s-template", im.Name)
	desiredSourceImage := fmt.Sprintf("projects/%s/global/images/%s", project, im.Name)

	it, err := service.InstanceTemplates.Get(project, desiredIt).Do()

	if err == nil {
		return it, nil
	}

	templates, err := service.InstanceTemplates.List(project).Do()

	if err != nil {
		return nil, err
	}

	for i := 0; i < len(templates.Items); i++ {
		name := templates.Items[i].Name

		if strings.Compare(name, itToClone) == 0 {
			it = templates.Items[i]
		}
	}

	if it == nil {
		return nil, errors.New(fmt.Sprintf("No template exists for identifier: %s", itToClone))
	}

	it.Name = desiredIt
	it.Properties.Disks[0].InitializeParams.SourceImage = desiredSourceImage

	_, err = service.InstanceTemplates.Insert(project, it).Do()

	if err != nil {
		return nil, err
	}

	return it, nil
}

func getImageToDeploy(cs *compute.Service, project, id string) (*compute.Image, error) {
	list, err := cs.Images.List(project).Do()

	if err != nil {
		return nil, err
	}

	c := len(list.Items)
	var im *compute.Image

	for i := 0; i < c; i++ {
		image := list.Items[i]
		// r[i] = image.Name
		if strings.Contains(image.Name, id) {
			im = image
		}
	}

	if im == nil {
		return nil, errors.New(fmt.Sprintf("No image exists for identifier: %s", id))
	}

	return im, nil
}

func updateInstanceGroupToNewTemplate(cs *compute.Service, it *compute.InstanceTemplate, project, zone, instanceGroup string) (*compute.InstanceGroupManager, error) {
	x := compute.InstanceGroupManagersSetInstanceTemplateRequest{
		InstanceTemplate: fmt.Sprintf("projects/%s/global/instanceTemplates/%s", project, it.Name),
	}

	_, err := cs.InstanceGroupManagers.SetInstanceTemplate(project, zone, instanceGroup, &x).Do()

	for count := 1; err != nil && count < 5; count++ {
		log.Printf("error from resize, %v", err)
		log.Printf("waiting, retry %d", count)
		time.Sleep(time.Second)
		_, err = cs.InstanceGroupManagers.SetInstanceTemplate(project, zone, instanceGroup, &x).Do()
	}

	if err != nil {
		return nil, err
	}

	igm, err := cs.InstanceGroupManagers.Get(project, zone, instanceGroup).Do()

	for count := 1; err != nil && count < 5; count++ {
		log.Printf("error from resize, %v", err)
		log.Printf("waiting, retry %d", count)
		time.Sleep(time.Second)
		igm, err = cs.InstanceGroupManagers.Get(project, zone, instanceGroup).Do()
	}

	if err != nil {
		return nil, err
	}

	for !strings.Contains(igm.InstanceTemplate, it.Name) {
		log.Printf("waiting for InstanceGroup %s to be updated. Got %s. Wanted %s", igm.Name, igm.InstanceTemplate, it.Name)
		time.Sleep(time.Second)

		igm, err = cs.InstanceGroupManagers.Get(project, zone, instanceGroup).Do()

		if err != nil {
			return nil, err
		}
	}

	log.Printf("updated %s with template %s", instanceGroup, it.Name)

	return igm, nil
}

func getName(iName string) string {
	a := strings.Split(iName, "/")
	return a[len(a)-1]
}

func waitUntilRunning(cs *compute.Service, igm *compute.InstanceGroupManager, project, zone string) error {
	lmi, err := cs.InstanceGroupManagers.ListManagedInstances(project, zone, igm.Name).Do()
	// var r compute.InstanceGroupsListInstancesRequest
	// ig, err := service.InstanceGroups.ListInstances(project, zone, igm.Name, &r).Do

	actioning := true
	count := 0
	// log.Printf("resp from get %v", lmi)
	// service.InstanceGroupManagers.
	for actioning == true && err == nil {
		actioning = false
		for _, item := range lmi.ManagedInstances {
			if item.CurrentAction != "NONE" {
				log.Printf("found template %v, state %s", getName(item.Instance), item.CurrentAction)
				actioning = true
			}
		}
		if actioning == false {
			break
		}
		count++
		log.Printf("waiting 5 seconds, retry %d", count)
		time.Sleep(time.Second * 5)
		lmi, err = cs.InstanceGroupManagers.ListManagedInstances(project, zone, igm.Name).Do()
	}

	if err != nil {
		return err
	}

	return nil
}

func resizeInstanceGroup(cs *compute.Service, igm *compute.InstanceGroupManager, project, zone string, size int64) error {
	log.Printf("setting to size %d", size)

	resp, err := cs.InstanceGroupManagers.Resize(project, zone, igm.Name, size).Do()
	// service.InstanceGroupManagers.Resize(project, zone, igm, 2)
	for count := 1; err != nil && count < 5; count++ {
		log.Printf("error from resize, %v", err)
		log.Printf("waiting, retry %d", count)
		time.Sleep(time.Second)
		resp, err = cs.InstanceGroupManagers.Resize(project, zone, igm.Name, size).Do()
	}

	if err != nil {
		return err
	}

	log.Printf("resp from resize %v", resp.Status)

	err = waitUntilRunning(cs, igm, project, zone)
	if err != nil {
		return err
	}

	// log.Printf("call response %v, request %v", f.Items, r.InstanceState)

	return nil
}

func recreateAndWaitForInstance(cs *compute.Service, igm *compute.InstanceGroupManager, iName, project, zone string) error {
	// a := strings.Split(iName, "/")
	// name := a[len(a)-1]

	r := compute.InstanceGroupManagersRecreateInstancesRequest{
		Instances: []string{
			iName,
		},
	}

	_, err := cs.InstanceGroupManagers.RecreateInstances(project, zone, igm.Name, &r).Do()

	if err != nil {
		return err
	}

	err = waitUntilRunning(cs, igm, project, zone)

	if err != nil {
		return err
	}

	return nil
}

func rolloutToManagedInstances(cs *compute.Service, igm *compute.InstanceGroupManager, mi []*compute.ManagedInstance, project, zone string) error {
	// server.

	// c := len(mi)
	// var err error
	for _, item := range mi {
		log.Printf("rolling out to %v", getName(item.Instance))
		err := recreateAndWaitForInstance(cs, igm, item.Instance, project, zone)

		if err != nil {
			return err
		}
		log.Printf("letting %q cool down", getName(item.Instance))
		time.Sleep(time.Second * 15)
	}

	return nil
}

func action(project, imageId, zone, instanceGroup, instanceTemplate string) error {
	// Use oauth2.NoContext if there isn't a good context to pass in.
	ctx := context.TODO()

	client, err := google.DefaultClient(ctx, compute.ComputeScope)
	if err != nil {
		log.Printf("error default context")
		return err
	}

	cs, err := compute.New(client)
	if err != nil {
		log.Printf("error computer service")
		return err
	}

	// 1. Setup an instance group with an image
	// 2. Get all the current instances for the instance-group
	// 3. Get information about the instance-group
	// 3. Change the instance-group to use the new instance-template
	// 4. Make sure number of instances atleast = 2
	// 5. One by one, go and delete an old instance until none left
	// 6. Set the number of instances back to its old value

	im, err := getImageToDeploy(cs, project, imageId)
	if err != nil {
		log.Printf("error getting image")
		return err
	}
	log.Printf("got image %v", im.Name)

	it, err := createInstanceTemplateWithImage(cs, im, instanceTemplate, project)
	if err != nil {
		return err
	}
	log.Printf("instance template %v", it.Name)

	igm, err := updateInstanceGroupToNewTemplate(cs, it, project, zone, instanceGroup)
	if err != nil {
		return err
	}
	log.Printf("instance group manager %v", igm.Name)

	lmi, err := cs.InstanceGroupManagers.ListManagedInstances(project, zone, igm.Name).Do()
	if err != nil {
		return err
	}

	if igm.TargetSize == 1 {
		log.Printf("WARNING: only 1 host available to rollout to, will most likely see service interuption")
		if strings.Contains(igm.Name, "prod") || strings.Contains(im.Name, "prod") {
			log.Fatalf("\tproduction deployment detected, failing")
		}
	}

	// err = resizeInstanceGroup(cs, igm, project, zone, igm.TargetSize+1)
	// if err != nil {
	// 	return err
	// }

	err = rolloutToManagedInstances(cs, igm, lmi.ManagedInstances, project, zone)
	if err != nil {
		return err
	}

	// err = resizeInstanceGroup(cs, igm, project, zone, igm.TargetSize)
	// if err != nil {
	// 	return err
	// }

	return nil
}

func main() {
	app := cli.NewApp()
	app.Name = "autogoog-rollout"
	app.Usage = "Automate the rollout of google images to an instance group"
	app.UsageText = "rollout [options]"

	if VERSION == "" {
		VERSION = "0.0.0"
	}
	app.Version = VERSION

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "project",
			Usage: "Google project",
		},
		cli.StringFlag{
			Name:  "zone",
			Usage: "Google zone to update",
		},
		cli.StringFlag{
			Name:  "image-id",
			Usage: "An identifier for selecting a google image to deploy",
		},
		cli.StringFlag{
			Name:  "instance-group",
			Usage: "Instance group to rollout image to",
		},
		cli.StringFlag{
			Name:  "instance-template",
			Usage: "Instance template to clone",
		},
	}

	app.Action = func(c *cli.Context) error {
		project := c.String("project")
		imageId := c.String("image-id")
		zone := c.String("zone")
		instanceGroup := c.String("instance-group")
		instanceTemplate := c.String("instance-template")

		if project == "" || imageId == "" || zone == "" || instanceGroup == "" || instanceTemplate == "" {
			return cli.ShowAppHelp(c)
		}

		if err := action(project, imageId, zone, instanceGroup, instanceTemplate); err != nil {
			log.Printf("error running command: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
		return nil
	}
	app.Run(os.Args)
}
