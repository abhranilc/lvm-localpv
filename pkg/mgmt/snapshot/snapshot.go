/*
Copyright 2021 The OpenEBS Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapshot

import (
	"fmt"
	"time"

	apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
	lvm "github.com/openebs/lvm-localpv/pkg/lvm"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

// isDeletionCandidate checks if a lvm snapshot is a deletion candidate.
func (c *SnapController) isDeletionCandidate(snap *apis.LVMSnapshot) bool {
	return snap.ObjectMeta.DeletionTimestamp != nil
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two.
func (c *SnapController) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the snap resource with this namespace/name
	snap, err := c.snapLister.LVMSnapshots(namespace).Get(name)
	if k8serror.IsNotFound(err) {
		runtime.HandleError(fmt.Errorf("lvm snapshot '%s' has been deleted", key))
		return nil
	}
	if err != nil {
		return err
	}
	snapCopy := snap.DeepCopy()
	err = c.syncSnap(snapCopy)
	return err
}

// enqueueSnap takes a LVMSnapshot resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than LVMSnapshot.
func (c *SnapController) enqueueSnap(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

// synSnap is the function which tries to converge to a desired state for the
// LVMSnapshot
func (c *SnapController) syncSnap(snap *apis.LVMSnapshot) error {
	var err error
	// LVMSnapshot should be deleted. Check if deletion timestamp is set
	if c.isDeletionCandidate(snap) {
		err = lvm.DestroySnapshot(snap)
		if err == nil {
			lvm.RemoveSnapFinalizer(snap)
		}
	} else {
		// if the status of the snapshot resource is Pending, then
		// we create the snapshot on the machine
		if snap.Status.State == lvm.LVMStatusPending {
			err = lvm.CreateSnapshot(snap)
			if err == nil {
				err = lvm.UpdateSnapInfo(snap)
			}
		}
	}
	return err
}

// addSnap is the add event handler for LVMSnapshot
func (c *SnapController) addSnap(obj interface{}) {
	snap, ok := obj.(*apis.LVMSnapshot)
	if !ok {
		runtime.HandleError(fmt.Errorf("Couldn't get snap object %#v", obj))
		return
	}

	if lvm.NodeID != snap.Spec.OwnerNodeID {
		return
	}
	klog.Infof("Got add event for Snap %s/%s", snap.Spec.VolGroup, snap.Name)
	c.enqueueSnap(snap)
}

// updateSnap is the update event handler for LVMSnapshot
func (c *SnapController) updateSnap(oldObj, newObj interface{}) {

	newSnap, ok := newObj.(*apis.LVMSnapshot)
	if !ok {
		runtime.HandleError(fmt.Errorf("Couldn't get snap object %#v", newSnap))
		return
	}

	if lvm.NodeID != newSnap.Spec.OwnerNodeID {
		return
	}

	// update on Snapshot CR does not make sense unless it is a deletion candidate
	if c.isDeletionCandidate(newSnap) {
		klog.Infof("Got update event for Snap %s/%s@%s", newSnap.Spec.VolGroup, newSnap.Labels[lvm.LVMVolKey], newSnap.Name)
		c.enqueueSnap(newSnap)
	}
}

// deleteSnap is the delete event handler for LVMSnapshot
func (c *SnapController) deleteSnap(obj interface{}) {
	snap, ok := obj.(*apis.LVMSnapshot)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		snap, ok = tombstone.Obj.(*apis.LVMSnapshot)
		if !ok {
			runtime.HandleError(fmt.Errorf("Tombstone contained object that is not a lvmsnap %#v", obj))
			return
		}
	}

	if lvm.NodeID != snap.Spec.OwnerNodeID {
		return
	}

	klog.Infof("Got delete event for Snap %s/%s@%s", snap.Spec.VolGroup, snap.Labels[lvm.LVMVolKey], snap.Name)
	c.enqueueSnap(snap)
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *SnapController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting Snap controller")

	// Wait for the k8s caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.snapSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	klog.Info("Starting Snap workers")
	// Launch worker to process Snap resources
	// Threadiness will decide the number of workers you want to launch to process work items from queue
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started Snap workers")
	<-stopCh
	klog.Info("Shutting down Snap workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *SnapController) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *SnapController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Snap resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}
