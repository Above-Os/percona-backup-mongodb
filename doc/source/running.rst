.. _pbm.running:

Running |pbm|
********************************************************************************

.. contents::
   :local:

Please see :ref:`pbm.auth` if you have not already. This will explain the
MongoDB user that needs to be created, and the connection method used by |pbm|.

Initial Setup
================================================================================

1. Determine the right MongoDB connection string for the |pbm.app| CLI.
   (See :ref:`pbm.auth.mdb_conn_string`) 
#. Use the |pbm.app| CLI to insert the config (especially the Remote Storage
   location and credentials information). See :ref:`pbm.config.initialize`
#. Start (or restart) the |pbm-agent| processes for all mongod nodes.


Start the |pbm-agent| processes
--------------------------------------------------------------------------------
After installing |pbm-agent| on the all the servers that have mongod nodes make
sure one instance of it is started for each mongod node.

E.g. Imagine you put configsvr nodes (listen port 27019) colocated on the same
servers as the first shard's mongod nodes (listen port 27018, replica set name
"sh1rs"). In this server there should be two 
|pbm-agent| processes, one connected to the shard
(e.g. "mongodb://username:password@localhost:27018/") and one to the configsvr
node (e.g. "mongodb://username:password@localhost:27019/").

It is best to use the packaged service scripts to run |pbm-agent|. After
adding the database connection configuration for them (see
:ref:`pbm.installation.service_init_scripts`), you can start the |pbm-agent|
service as below:

.. code-block:: bash

   $ sudo systemctl start pbm-agent
   $ sudo systemctl status pbm-agent

For reference an example of starting pbm-agent manually is shown below. The
output is redirected to a file and the process is backgrounded. Alternatively
you can run it on a shell terminal temporarily if you want to observe and/or
debug the startup from the log messages.

.. code-block:: bash

   $ nohup pbm-agent --mongodb-uri "mongodb://username:password@localhost:27018/" > /data/mdb_node_xyz/pbm-agent.$(hostname -s).27018.log 2>&1 &

.. tip::
   
   Running as the ``mongod`` user would be the most intuitive and convenient way.
   But if you want it can be another user.

When a message *"pbm agent is listening for the commands"* is printed to the
|pbm-agent| log file it confirms it connected to its mongod successfully.


.. _pbm-agent.log:

How to see the pbm-agent log
--------------------------------------------------------------------------------

With the packaged systemd service the log output to stdout is captured by
systemd's default redirection to systemd-journald. You can view it with the
command below. See `man journalctl` for useful options such as '--lines',
'--follow', etc.

.. code-block:: bash

   ~$ journalctl -u pbm-agent.service
   -- Logs begin at Tue 2019-10-22 09:31:34 JST. --
   Jan 22 15:59:14 akira-x1 systemd[1]: Started pbm-agent.
   Jan 22 15:59:14 akira-x1 pbm-agent[3579]: pbm agent is listening for the commands
   ...
   ...

If you started pbm-agent manually see the file you redirected stdout and stderr
to.

Running |pbm|
================================================================================
Provide the MongoDB URI connection string for |pbm.app|. This allows you to call |pbm.app| commands without the :option:`--mongodb-uri` flag.

Use the following command:

.. code-block:: guess
 
   export PBM_MONGODB_URI="mongodb://pbmuser:secretpwd@localhost:27018/"

For more information what connection string to specify, refer to :ref:`pbm.auth.pbm.app_conn_string` section.

Running |pbm.app| Commands
================================================================================

|pbm.app| is the command line utility to control the backup system.

Configuring a Remote Store for Backup and Restore Operations
--------------------------------------------------------------------------------

This must be done once, at installation or re-installation time, before backups can
be listed, made, or restored. Please see :ref:`pbm.config`.

.. _pbm.running.backup.listing:

Listing all backups
--------------------------------------------------------------------------------

.. include:: .res/code-block/bash/pbm-list-mongodb-uri.txt

.. admonition:: Sample output

   .. code-block:: text

      2020-07-10T07:04:14Z
      2020-07-09T07:03:50Z
      2020-07-08T07:04:21Z
      2020-07-07T07:04:18Z

.. _pbm.running.backup.starting: 

Starting a backup
--------------------------------------------------------------------------------

.. include:: .res/code-block/bash/pbm-backup-mongodb-uri.txt

.. rubric:: Starting a backup with compression

.. include:: .res/code-block/bash/pbm-backup-compression.txt

``s2`` is the default compression type. Other supported compression types are: ``gzip``,
``snappy``, ``lz4``, ``pgzip``.  The ``none`` value means no compression is done during
backup.

.. important::

   For PBM v1.0 (only) before running |pbm-backup| on a cluster stop the
   balancer.

Checking an in-progress backup
--------------------------------------------------------------------------------

Run the |pbm-list| command and you will see the running backup listed with a
'In progress' label. When that is absent the backup is complete.

.. _pbm.running.backup.restoring: 

Restoring a backup
--------------------------------------------------------------------------------

To restore a backup that you have made using |pbm-backup| you should use the
|pbm-restore| command supplying the time stamp of the backup that you intend to
restore.

.. important::

   Before running |pbm-restore| on a cluster stop the
   balancer.

.. important::

   If you enabled :term:`Point-in-Time Recovery`, disable it before running |pbm-restore|. This is because |PITR| incremental backups and restore are incompatible operations and cannot be run together. 

.. important::

   Whilst the restore is running, clients should be stopped from accessing the
   database. The data will naturally be incomplete whilst the restore is in
   progress, and writes they make will cause the final restored data to differ
   from the backed-up data. In a cluster's restore the simplest way would be to
   shutdown all mongos nodes.

.. important::

   |pbm| is designed to be a full-database restore tool. As of version <=1.x it
   will perform a full all-databases, all collections restore and does not
   offer an option to restore only a subset of collections in the backup, as
   MongoDB's mongodump tool does. But to avoid surprising mongodump users |pbm|
   as of now (versions 1.x) replicates mongodump's behavior to only drop
   collections in the backup. It does not drop collections that are created new
   after the time of the backup and before the restore. Run a db.dropDatabase()
   manually in all non-system databases (i.e. all databases except "local",
   "config" and "admin") before running |pbm-restore| if you want to guarantee
   the post-restore database only includes collections that are in the backup.

.. include:: .res/code-block/bash/pbm-restore-mongodb-uri.txt

After a cluster's restore is complete, restart all ``mongos`` nodes to reload the sharding metadata.

Starting from v1.3.2, the |pbm| config includes the restore options to adjust the memory consumption by the |pbm-agent| in environments with tight memory bounds. This allows preventing out of memory errors during the restore operation. 

.. code-block:: yaml

   restore:
     batchSize: 500
     numInsertionWorkers: 10

.. option:: batchSize
   
   :default: 500

   The number of documents to buffer. 

.. option:: numInsertionWorkers 

   :default: 10

   The number of workers that add the documents to buffer. 

The default values were adjusted to fit the setups with the memory allocation of 1GB and less for the agent. 

.. note:: 

  The lower the values, the less memory is allocated for the restore. However, the performance decreases too.

.. _pbm.cancel.backup:

Canceling a backup
--------------------------------------------------------------------------------

You can cancel a running backup if, for example, you want to do
another maintenance and don't want to wait for the large backup to finish first.

To cancel the backup, use the |pbm-cancel-backup| command.

.. code-block:: bash

  $ pbm cancel-backup
  Backup cancellation has started

After the command execution, the backup is marked as canceled in the |pbm-list| output:

.. code-block:: bash

  $ pbm list
  ...
  2020-04-30T18:05:26Z	Canceled at 2020-04-30T18:05:37Z
  
.. _pbm.backup.delete:

Deleting backups
--------------------------------------------------------------------------------

Use the |pbm-delete-backup| command to delete a specified backup or all backups
older than the specified time.

The command deletes the backup regardless of the remote storage used:
either S3-compatible or a filesystem-type remote storage.

.. note::

  You can only delete a backup that is not running (has the "done" or the "error" state). 

To delete a backup, specify the ``<backup_name>`` from the the |pbm-list|
output as an argument. 

.. include:: .res/code-block/bash/pbm-delete-backup.txt

By default, the |pbm-delete-backup| command asks for your confirmation
to proceed with the deletion. To bypass it, add the ``-f`` or
``--force`` flag.

.. code-block:: bash

  $ pbm delete-backup --force 2020-04-20T13:45:59Z

To delete backups that were created before the specified time, pass the ``--older-than`` flag to the |pbm-delete-backup|
command. Specify the timestamp as an argument
for the |pbm-delete-backup| command in the following format:

* ``%Y-%M-%DT%H:%M:%S`` (e.g. 2020-04-20T13:13:20) or
* ``%Y-%M-%D`` (e.g. 2020-04-20).

.. include:: .res/code-block/bash/pbm-delete-backup-older-than-timestamp.txt

.. include:: .res/replace.txt
